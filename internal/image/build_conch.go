package image

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/openeuler/Conch/internal/image/conchbuild"
	"github.com/openeuler/Conch/internal/image/conchbuild/erofs"
	"github.com/openeuler/Conch/internal/image/conchbuild/kernel"
	"github.com/openeuler/Conch/internal/image/conchbuild/ocipublisher"
	"github.com/openeuler/Conch/internal/image/conchbuild/rootfs"
	"github.com/sirupsen/logrus"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// BuildRequest is the unified input for conch build (docs/CONCH_BUILD_API.md).
// If DockerfilePath and ContextDir are empty, BuildahArgs is parsed as `buildah bud`-style argv (including -f and trailing context).
// The current implementation requires a local filesystem context because Dockerfile preprocessing reads local files.
type BuildRequest struct {
	ContextDir      string
	DockerfilePath  string
	BuildahArgs     []string
	ConfigPath      string
	ConchAPIBaseURL string // default http://localhost:4063; also see BUILDAH_CONCH_API_URL
	Stdout          io.Writer
	Stderr          io.Writer
}

// BuildResult is returned after a successful conch build.
type BuildResult struct {
	RootfsImageID      string
	FinalImageID       string
	RootfsImageRef     string
	VMImageRef         string
	SandboxImageRef    string
	PmemRootfsImageRef string
	BootIndexTag       string
	UsedKernel         bool
	UsedIndex          bool
	UsedSnap           bool
}

// BuildahBuildRequest builds a standard OCI rootfs image (preprocessed Dockerfile).
type BuildahBuildRequest struct {
	ContextDir     string
	DockerfilePath string
	BuildahArgs    []string
	Stdout         io.Writer
	Stderr         io.Writer
}

// BuildahBuildResult holds the image ID from buildah bud (--iidfile).
type BuildahBuildResult struct {
	ImageID string
}

// SnapRequest drives KERNEL vm image + SNAP post-processing (after rootfs is built).
type SnapRequest struct {
	ContextDir      string
	KernelFile      string
	InitrdFile      string
	RootfsImageID   string
	RootfsImageRef  string
	BootIndexTag    string
	VMImageRef      string
	ConfigPath      string
	ConchAPIBaseURL string
	Stdout          io.Writer
}

// SnapResult holds the published boot index digest when SNAP completes.
type SnapResult struct {
	BootIndexDigest string
	BootIndexTag    string
	PmemRootfsRef   string
}

// BuildWithConchExtensions is the main entry for `conch build`: preprocess, buildah bud, optional Conch image assembly.
func BuildWithConchExtensions(ctx context.Context, req BuildRequest) (BuildResult, error) {
	out, errOut := req.Stdout, req.Stderr
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}

	dockerfile := req.DockerfilePath
	contextDir := req.ContextDir
	forward := append([]string(nil), req.BuildahArgs...)

	if dockerfile == "" || contextDir == "" {
		var err error
		dockerfile, contextDir, forward, err = splitDockerfileAndContext(req.BuildahArgs)
		if err != nil {
			return BuildResult{}, err
		}
	}

	absDockerfile, err := filepath.Abs(dockerfile)
	if err != nil {
		return BuildResult{}, err
	}
	absCtx, err := filepath.Abs(contextDir)
	if err != nil {
		return BuildResult{}, err
	}

	pre, err := PreprocessDockerfile(absDockerfile, absCtx)
	if err != nil {
		return BuildResult{}, err
	}
	defer os.Remove(pre.TempDockerfile)
	logrus.Infof("[conch build] Dockerfile preprocess completed")
	if pre.Plan.KernelFile != "" {
		logrus.Infof("[conch build] KERNEL detected: kernel=%s initrd=%s", pre.Plan.KernelFile, pre.Plan.InitrdFile)
	}
	if pre.Plan.NeedSnap {
		logrus.Infof("[conch build] SNAP detected: snapshot flow enabled")
	} else if pre.Plan.NeedIndex {
		logrus.Infof("[conch build] INDEX detected: sandbox-image flow enabled")
	} else {
		logrus.Infof("[conch build] INDEX/SNAP not detected: rootfs-only build")
	}

	patched := patchDockerfileFlag(forward, pre.TempDockerfile)
	requestedTag := firstImageTag(patched)
	rootfsRef := requestedTag
	bootIndexTag := ""
	sandboxImageTag := ""
	if pre.Plan.NeedSnap {
		bootIndexTag = requestedTag
		if bootIndexTag == "" {
			bootIndexTag = "localhost/conch/sandbox-snapshot:latest"
		}
		rootfsRef = fmt.Sprintf("localhost/conch-rootfs:%d", time.Now().UnixNano())
		patched = replaceImageTag(patched, rootfsRef)
		logrus.Infof("[conch build] using temporary rootfs tag %s for SNAP flow", rootfsRef)
	} else if pre.Plan.NeedIndex {
		sandboxImageTag = requestedTag
		if sandboxImageTag == "" {
			sandboxImageTag = "localhost/conch/sandbox-image:latest"
		}
		rootfsRef = fmt.Sprintf("localhost/conch-rootfs:%d", time.Now().UnixNano())
		patched = replaceImageTag(patched, rootfsRef)
		logrus.Infof("[conch build] using temporary rootfs tag %s for INDEX flow", rootfsRef)
	}

	iidFile, err := os.CreateTemp("", "conch-iid-*")
	if err != nil {
		return BuildResult{}, err
	}
	iidPath := iidFile.Name()
	_ = iidFile.Close()
	defer os.Remove(iidPath)

	patched = append(patched, "--iidfile", iidPath)
	patched = append(patched, absCtx)

	bres, err := BuildOCIImage(ctx, BuildahBuildRequest{
		ContextDir:     absCtx,
		DockerfilePath: pre.TempDockerfile,
		BuildahArgs:    patched,
		Stdout:         out,
		Stderr:         errOut,
	})
	if err != nil {
		return BuildResult{}, fmt.Errorf("E_BUILDAH_BUILD: %w", err)
	}
	imageID := bres.ImageID
	logrus.Infof("[conch build] rootfs image id: %s", imageID)

	storeOpts, err := storage.DefaultStoreOptions()
	if err != nil {
		return BuildResult{}, fmt.Errorf("default store options: %w", err)
	}
	store, err := storage.GetStore(storeOpts)
	if err != nil {
		return BuildResult{}, fmt.Errorf("get store: %w", err)
	}
	defer func() { _, _ = store.Shutdown(false) }()

	sysCtx := &types.SystemContext{}
	result := BuildResult{RootfsImageID: imageID, FinalImageID: imageID, RootfsImageRef: rootfsRef}
	vmTag := os.Getenv("CONCH_KERNEL_IMAGE_TAG")
	if vmTag == "" {
		vmTag = "localhost/conch/kernel:latest"
	}
	result.VMImageRef = vmTag

	apiBase := req.ConchAPIBaseURL
	if apiBase == "" {
		apiBase = os.Getenv("BUILDAH_CONCH_API_URL")
	}

	if pre.Plan.KernelFile != "" && pre.Plan.InitrdFile != "" {
		if _, err := kernel.BuildKernelImage(ctx, store, absCtx, pre.Plan.KernelFile, pre.Plan.InitrdFile, vmTag); err != nil {
			return BuildResult{}, fmt.Errorf("E_KERNEL_BUILD: %w", err)
		}
		result.UsedKernel = true
		logrus.Infof("Built kernel image tag=%s", vmTag)
		logrus.Infof("[conch build] kernel image built: tag=%s", vmTag)
	}

	if pre.Plan.NeedSnap {
		snapReq := SnapRequest{
			ContextDir:      absCtx,
			KernelFile:      pre.Plan.KernelFile,
			InitrdFile:      pre.Plan.InitrdFile,
			RootfsImageID:   imageID,
			RootfsImageRef:  rootfsRef,
			BootIndexTag:    bootIndexTag,
			VMImageRef:      vmTag,
			ConfigPath:      req.ConfigPath,
			ConchAPIBaseURL: apiBase,
			Stdout:          out,
		}
		sres, err := ExecuteSnapFlow(ctx, store, sysCtx, snapReq)
		if err != nil {
			return BuildResult{}, err
		}
		result.UsedSnap = true
		result.FinalImageID = sres.BootIndexDigest
		result.PmemRootfsImageRef = sres.PmemRootfsRef
		result.BootIndexTag = sres.BootIndexTag
		logrus.Infof("[conch build] boot index digest: %s", result.FinalImageID)
	}
	if pre.Plan.NeedIndex {
		if pre.Plan.KernelFile == "" || pre.Plan.InitrdFile == "" {
			return BuildResult{}, fmt.Errorf("E_INDEX_DEPENDS_KERNEL: INDEX requires KERNEL paths")
		}
		pmemRootfsRef, err := buildPmemRootfsForIndex(ctx, store, imageID, rootfsRef, sysCtx)
		if err != nil {
			return BuildResult{}, fmt.Errorf("E_INDEX_ROOTFS_BUILD: %w", err)
		}
		result.PmemRootfsImageRef = pmemRootfsRef

		indexDigest, err := ocipublisher.PublishSandboxImage(ctx, store, pmemRootfsRef, vmTag, sandboxImageTag)
		if err != nil {
			return BuildResult{}, fmt.Errorf("E_INDEX_PUBLISH: %w", err)
		}
		result.UsedIndex = true
		result.FinalImageID = indexDigest.String()
		result.SandboxImageRef = sandboxImageTag
		logrus.Infof("[conch build] sandbox image built: tag=%s digest=%s", sandboxImageTag, result.FinalImageID)
	}
	logrus.Infof("[conch build] final image id: %s", result.FinalImageID)
	printBuildResultSummary(out, result)
	return result, nil
}

func printBuildResultSummary(out io.Writer, result BuildResult) {
	if out == nil {
		return
	}

	printLine := func(label, value string) {
		fmt.Fprintf(out, "  %-20s %s\n", label+":", value)
	}

	if result.UsedSnap {
		fmt.Fprintln(out, "Build outputs:")
		if result.RootfsImageRef != "" {
			printLine("RootFS build image", result.RootfsImageRef)
		}
		if result.PmemRootfsImageRef != "" {
			printLine("PMEM RootFS image", result.PmemRootfsImageRef)
		}
		if result.VMImageRef != "" {
			printLine("Kernel image", result.VMImageRef)
		}
		if result.BootIndexTag != "" {
			printLine("Sandbox snapshot", result.BootIndexTag)
			printLine("Push command", fmt.Sprintf("buildah manifest push --all %s docker://<registry>/<repository>:<tag>", result.BootIndexTag))
		}
		return
	}

	if result.UsedIndex {
		fmt.Fprintln(out, "Build outputs:")
		if result.RootfsImageRef != "" {
			printLine("RootFS build image", result.RootfsImageRef)
		}
		if result.PmemRootfsImageRef != "" {
			printLine("PMEM RootFS image", result.PmemRootfsImageRef)
		}
		if result.VMImageRef != "" {
			printLine("Kernel image", result.VMImageRef)
		}
		if result.SandboxImageRef != "" {
			printLine("Sandbox image", result.SandboxImageRef)
			printLine("Push command", fmt.Sprintf("buildah manifest push --all %s docker://<registry>/<repository>:<tag>", result.SandboxImageRef))
		}
		return
	}

	if result.RootfsImageRef != "" {
		fmt.Fprintln(out, "Build outputs:")
		printLine("RootFS image", result.RootfsImageRef)
	}
}

func buildPmemRootfsForIndex(ctx context.Context, store storage.Store, imageID, imageRef string, sysCtx *types.SystemContext) (string, error) {
	tmpDir, err := os.MkdirTemp("", "conch-index-erofs-*")
	if err != nil {
		return "", fmt.Errorf("create temp erofs output dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	erofsPath, err := erofs.ConvertImageToEROFSDirect(ctx, store, imageID, imageRef, sysCtx, tmpDir)
	if err != nil {
		return "", fmt.Errorf("convert rootfs image to EROFS: %w", err)
	}

	rootfsTag := fmt.Sprintf("localhost/conch/pmem-rootfs:index-%d", time.Now().UnixNano())
	if _, err := rootfs.BuildRootfsImage(ctx, store, []string{erofsPath}, rootfsTag); err != nil {
		return "", fmt.Errorf("build pmem-rootfs image: %w", err)
	}
	return rootfsTag, nil
}

// BuildOCIImage runs `buildah bud` (CONCH_BUILDAH_BIN, default `buildah`) with given args.
// BuildahArgs must already include context path and `--iidfile` if callers need image ID.
func BuildOCIImage(ctx context.Context, req BuildahBuildRequest) (BuildahBuildResult, error) {
	if req.Stdout == nil {
		req.Stdout = os.Stdout
	}
	if req.Stderr == nil {
		req.Stderr = os.Stderr
	}
	buildahBin := os.Getenv("CONCH_BUILDAH_BIN")
	if buildahBin == "" {
		buildahBin = "buildah"
	}
	cmd := exec.CommandContext(ctx, buildahBin, append([]string{"bud"}, req.BuildahArgs...)...)
	cmd.Stdout = req.Stdout
	cmd.Stderr = req.Stderr
	cmd.Stdin = os.Stdin
	cmd.Dir = req.ContextDir
	if err := cmd.Run(); err != nil {
		return BuildahBuildResult{}, err
	}
	// Caller must pass --iidfile in BuildahArgs; find and read it
	iidPath := ""
	args := req.BuildahArgs
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--iidfile" {
			iidPath = args[i+1]
			break
		}
	}
	if iidPath == "" {
		return BuildahBuildResult{}, fmt.Errorf("BuildOCIImage: missing --iidfile in BuildahArgs")
	}
	rawID, err := os.ReadFile(iidPath)
	if err != nil {
		return BuildahBuildResult{}, fmt.Errorf("read iidfile: %w", err)
	}
	imageID := strings.TrimSpace(string(rawID))
	imageID = strings.TrimPrefix(imageID, "sha256:")
	if imageID == "" {
		return BuildahBuildResult{}, fmt.Errorf("empty image id from iidfile")
	}
	return BuildahBuildResult{ImageID: imageID}, nil
}

// ExecuteSnapFlow runs containerd sync, conchd sandbox, and boot index publish.
func ExecuteSnapFlow(ctx context.Context, store storage.Store, sysCtx *types.SystemContext, req SnapRequest) (SnapResult, error) {
	if req.KernelFile == "" || req.InitrdFile == "" {
		return SnapResult{}, fmt.Errorf("E_SNAP_DEPENDS_KERNEL: SNAP requires KERNEL paths")
	}
	final, err := conchbuild.ExecuteSNAP(ctx, conchbuild.SNAPOpts{
		Store:           store,
		ContextDir:      req.ContextDir,
		KernelArgs:      []string{req.KernelFile, req.InitrdFile},
		ImageID:         req.RootfsImageID,
		ImageRef:        req.RootfsImageRef,
		BootIndexTag:    req.BootIndexTag,
		VMImageRef:      req.VMImageRef,
		ConfigPath:      req.ConfigPath,
		SystemContext:   sysCtx,
		Out:             req.Stdout,
		ConchAPIBaseURL: req.ConchAPIBaseURL,
	})
	if err != nil {
		return SnapResult{}, fmt.Errorf("E_SNAPSHOT_LINK or E_CONCH_API or E_BUNDLE_PUBLISH: %w", err)
	}
	return SnapResult{
		BootIndexDigest: final.BootIndexDigest,
		BootIndexTag:    final.BootIndexTag,
		PmemRootfsRef:   final.PmemRootfsRef,
	}, nil
}

// splitDockerfileAndContext finds -f/--file and optional trailing CONTEXT from bud-like args.
// The last token is treated as context only when it looks like a path (not e.g. --build-arg values like FOO=bar).
func splitDockerfileAndContext(args []string) (dockerfile string, contextDir string, rest []string, err error) {
	rest = append([]string(nil), args...)

	for i := 0; i < len(rest); {
		switch {
		case rest[i] == "-f" || rest[i] == "--file":
			if i+1 >= len(rest) {
				return "", "", nil, fmt.Errorf("missing value for %s", rest[i])
			}
			dockerfile = rest[i+1]
			rest = append(rest[:i], rest[i+2:]...)
		case strings.HasPrefix(rest[i], "--file="):
			dockerfile = strings.TrimPrefix(rest[i], "--file=")
			if dockerfile == "" {
				return "", "", nil, fmt.Errorf("empty --file=")
			}
			rest = append(rest[:i], rest[i+1:]...)
		default:
			i++
		}
	}

	ctx := "."
	if len(rest) > 0 {
		last := rest[len(rest)-1]
		if isLikelyBuildContextArg(last) {
			ctx = last
			rest = rest[:len(rest)-1]
		}
	}
	if strings.Contains(ctx, "://") {
		return "", "", nil, fmt.Errorf("remote build context %q is not supported by conch build; use a local context directory", ctx)
	}

	if dockerfile == "" {
		for _, name := range []string{"Dockerfile", "Containerfile"} {
			candidate := filepath.Join(ctx, name)
			if st, stErr := os.Stat(candidate); stErr == nil && !st.IsDir() {
				dockerfile = candidate
				break
			}
		}
		if dockerfile == "" {
			return "", "", nil, fmt.Errorf("no Dockerfile specified (-f) and none found in context %q", ctx)
		}
	}

	return dockerfile, ctx, rest, nil
}

// isLikelyBuildContextArg mirrors buildah/docker behavior for local-path contexts:
// the trailing positional context looks like a path, not a bare KEY=value or other non-path token.
func isLikelyBuildContextArg(arg string) bool {
	if arg == "" || strings.HasPrefix(arg, "-") {
		return false
	}
	if arg == "." || arg == ".." {
		return true
	}
	if filepath.IsAbs(arg) {
		return true
	}
	if strings.Contains(arg, "/") || strings.Contains(arg, `\`) {
		return true
	}
	if fi, err := os.Stat(arg); err == nil && fi.IsDir() {
		return true
	}
	return false
}

func patchDockerfileFlag(args []string, newPath string) []string {
	out := make([]string, 0, len(args)+2)
	replaced := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-f" || a == "--file" {
			out = append(out, a, newPath)
			if i+1 < len(args) {
				i++
			}
			replaced = true
			continue
		}
		if strings.HasPrefix(a, "--file=") {
			out = append(out, "--file="+newPath)
			replaced = true
			continue
		}
		out = append(out, a)
	}
	if !replaced {
		out = append([]string{"-f", newPath}, out...)
	}
	return out
}

func firstImageTag(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-t" || args[i] == "--tag":
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		case strings.HasPrefix(args[i], "--tag="):
			return strings.TrimPrefix(args[i], "--tag=")
		}
	}
	return ""
}

func replaceImageTag(args []string, newTag string) []string {
	out := make([]string, 0, len(args)+2)
	replaced := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-t" || args[i] == "--tag":
			out = append(out, args[i], newTag)
			if i+1 < len(args) {
				i++
			}
			replaced = true
		case strings.HasPrefix(args[i], "--tag="):
			out = append(out, "--tag="+newTag)
			replaced = true
		default:
			out = append(out, args[i])
		}
	}
	if !replaced {
		out = append([]string{"-t", newTag}, out...)
	}
	return out
}
