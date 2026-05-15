package conchbuild

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/image/conchbuild/client"
	"github.com/openeuler/Conch/internal/image/conchbuild/erofs"
	"github.com/openeuler/Conch/internal/image/conchbuild/export"
	"github.com/openeuler/Conch/internal/image/conchbuild/ocipublisher"
	"github.com/openeuler/Conch/internal/image/conchbuild/rootfs"
	"github.com/sirupsen/logrus"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// SNAPOpts holds parameters for ExecuteSNAP.
type SNAPOpts struct {
	Store           storage.Store
	ContextDir      string
	KernelArgs      []string
	ImageID         string
	ImageRef        string
	BootIndexTag    string
	VMImageRef      string
	SystemContext   *types.SystemContext
	Out             io.Writer
	ConfigPath      string
	ConchAPIBaseURL string // optional; overrides BUILDAH_CONCH_API_URL / CONCHD_* when non-empty
}

// Result holds SNAP flow outputs needed by the caller.
type Result struct {
	BootIndexDigest string
	BootIndexTag    string
	PmemRootfsRef   string
}

// SnapshotExportOpts holds parameters for exporting an existing rootfs snapshot
// or sandbox into a sandbox-snapshot image.
type SnapshotExportOpts struct {
	Store            storage.Store
	BootIndexTag     string
	ConfigPath       string
	ConchAPIBaseURL  string
	RootfsSnapshotID string
	SandboxID        string
}

func progressf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	logrus.Infof("%s", msg)
	fmt.Fprintln(os.Stdout, msg)
}

// ExecuteSNAP runs the full SNAP flow: sync to containerd, call Conch Create/Pause,
// resolve rootfs/mem/vm paths, and publish a sandbox snapshot image. Returns the new imageID (index digest).
func ExecuteSNAP(ctx context.Context, opts SNAPOpts) (result Result, err error) {
	if len(opts.KernelArgs) != 2 {
		return Result{}, fmt.Errorf("SNAP instruction requires KERNEL instruction; add KERNEL <kernel_file> <initrd_file> before SNAP (e.g. KERNEL vmlinuz conch.initrd)")
	}

	containerdNamespace := resolveConchNamespace(opts.ConfigPath)
	conchClient := client.NewClientWithConfig(opts.ConchAPIBaseURL, opts.ConfigPath)

	// Always convert OCI rootfs to EROFS; conchd resolves the runtime paths from
	// the synced image snapshots during image_name-based sandbox creation.
	// If CONCH_EROFS_OUTPUT_DIR is empty, create a temp output directory for this build.
	erofsOut := os.Getenv("CONCH_EROFS_OUTPUT_DIR")
	cleanupErofsOut := false
	if erofsOut == "" {
		tmpOut, err := os.MkdirTemp("", "conch-erofs-*")
		if err != nil {
			return Result{}, fmt.Errorf("create temp erofs output dir: %w", err)
		}
		erofsOut = tmpOut
		cleanupErofsOut = true
	}
	erofsOut = filepath.Clean(erofsOut)
	if err := os.MkdirAll(erofsOut, 0o755); err != nil {
		return Result{}, fmt.Errorf("create erofs output dir: %w", err)
	}
	if cleanupErofsOut {
		defer func() {
			if rmErr := os.RemoveAll(erofsOut); rmErr != nil {
				logrus.Warnf("cleanup temp erofs output dir %s: %v", erofsOut, rmErr)
			}
		}()
	}

	var rootfsLayers []string
	if os.Getenv("CONCH_EROFS_PER_LAYER") == "1" {
		logrus.Infof("[conch build] EROFS mode: per-layer (output=%s)", erofsOut)
		// Per-layer: export to tar, convert each layer (compat path).
		layers, err := erofs.ConvertImageToEROFS(opts.Store, opts.ImageID, opts.SystemContext, erofsOut)
		if err != nil {
			return Result{}, fmt.Errorf("OCI->EROFS conversion failed: %w", err)
		}
		if len(layers) == 0 {
			return Result{}, fmt.Errorf("OCI->EROFS conversion produced no layers")
		}
		logrus.Infof("OCI->EROFS conversion complete: %d layers in %s", len(layers), erofsOut)
		rootfsLayers = append(rootfsLayers, layers...)
		logrus.Infof("[conch build] EROFS disk path: %s", layers[0])
	} else {
		logrus.Infof("[conch build] EROFS mode: direct (output=%s)", erofsOut)
		// Direct: mount merged rootfs and run mkfs.erofs on directory.
		destPath, err := erofs.ConvertImageToEROFSDirect(ctx, opts.Store, opts.ImageID, opts.ImageRef, opts.SystemContext, erofsOut)
		if err != nil {
			return Result{}, fmt.Errorf("OCI->EROFS conversion failed: %w", err)
		}
		logrus.Infof("OCI->EROFS conversion complete: %s", destPath)
		rootfsLayers = append(rootfsLayers, destPath)
		logrus.Infof("[conch build] EROFS disk path: %s", destPath)
	}

	rootfsImageRef := "localhost/conch/pmem-rootfs:latest"
	if _, err := rootfs.BuildRootfsImage(ctx, opts.Store, rootfsLayers, rootfsImageRef); err != nil {
		return Result{}, fmt.Errorf("build pmem-rootfs image: %w", err)
	}

	rootfsImport, err := syncImageViaConchd(ctx, conchClient, opts.Store, "", rootfsImageRef, "buildah-oci-rootfs:latest", containerdNamespace, opts.SystemContext)
	if err != nil {
		return Result{}, fmt.Errorf("failed to sync image %s to containerd: %w", rootfsImageRef, err)
	}
	rootfsSnapshotID := rootfsImport.SnapshotKey
	rootfsImageName := rootfsImport.ImageName
	logrus.Infof("Successfully synced image %s to containerd (imageName=%s, snapshot=%s)", rootfsImageRef, rootfsImageName, rootfsSnapshotID)
	logrus.Infof("[conch build] containerd sync: image=%s snapshot=%s", rootfsImageName, rootfsSnapshotID)

	if opts.VMImageRef == "" {
		return Result{}, fmt.Errorf("kernel image ref is required for SNAP flow")
	}
	vmImport, err := syncImageViaConchd(ctx, conchClient, opts.Store, "", opts.VMImageRef, "buildah-oci-vm:latest", containerdNamespace, opts.SystemContext)
	if err != nil {
		return Result{}, fmt.Errorf("failed to sync kernel image %s to containerd: %w", opts.VMImageRef, err)
	}
	vmSnapshotID := vmImport.SnapshotKey
	if err := conchClient.LinkRootfsSnapshotToVM(ctx, client.LinkSnapshotVMRequest{
		RootfsSnapshotID: rootfsSnapshotID,
		VMSnapshotID:     vmSnapshotID,
		Namespace:        containerdNamespace,
	}); err != nil {
		return Result{}, fmt.Errorf("link rootfs snapshot to sandbox snapshot: %w", err)
	}
	logrus.Infof("[conch build] linked rootfs snapshot %s -> sandbox snapshot %s", rootfsSnapshotID, vmSnapshotID)

	sandboxID := client.GenSandboxID()

	if err := conchClient.CreateSandbox(rootfsImageName, sandboxID, client.DefaultRamMB); err != nil {
		return Result{}, fmt.Errorf("Conch CreateSandbox failed: %w", err)
	}
	logrus.Infof("Conch sandbox %s created, VM started", sandboxID)

	rootfsCommitName, err := conchClient.PauseSandbox(sandboxID, containerdNamespace)
	if err != nil {
		return Result{}, fmt.Errorf("Conch PauseSandbox failed: %w", err)
	}
	logrus.Infof("Conch Pause complete. Rootfs snapshot: %s", rootfsCommitName)
	logrus.Infof("[conch build] conch pause rootfs snapshot: %s", rootfsCommitName)

	exported, err := exportSnapshotBundleFromRootfs(ctx, conchClient, opts.Store, rootfsCommitName, opts.BootIndexTag, containerdNamespace)
	if err != nil {
		return Result{}, err
	}
	exported.PmemRootfsRef = rootfsImageRef
	return exported, nil
}

// ExportSnapshot exports a sandbox-snapshot image from an existing rootfs
// snapshot ID or by pausing an existing sandbox ID.
func ExportSnapshot(ctx context.Context, opts SnapshotExportOpts) (Result, error) {
	if opts.Store == nil {
		return Result{}, fmt.Errorf("storage store is required")
	}
	if (opts.RootfsSnapshotID == "") == (opts.SandboxID == "") {
		return Result{}, fmt.Errorf("exactly one of --snapshot-id or --sandbox-id is required")
	}

	containerdNamespace := resolveConchNamespace(opts.ConfigPath)
	conchClient := client.NewClientWithConfig(opts.ConchAPIBaseURL, opts.ConfigPath)

	rootfsSnapshotID := opts.RootfsSnapshotID
	if rootfsSnapshotID == "" {
		progressf("[1/5] pausing sandbox %s to create rootfs snapshot...", opts.SandboxID)
		var err error
		rootfsSnapshotID, err = conchClient.PauseSandbox(opts.SandboxID, containerdNamespace)
		if err != nil {
			return Result{}, fmt.Errorf("Conch PauseSandbox failed: %w", err)
		}
		logrus.Infof("[conch snapshot export] conch pause rootfs snapshot: %s", rootfsSnapshotID)
	} else {
		progressf("[1/5] using existing rootfs snapshot %s...", rootfsSnapshotID)
	}

	return exportSnapshotBundleFromRootfs(ctx, conchClient, opts.Store, rootfsSnapshotID, opts.BootIndexTag, containerdNamespace)
}

func exportSnapshotBundleFromRootfs(ctx context.Context, conchClient *client.Client, store storage.Store, rootfsSnapshotID, bootIndexTag, namespace string) (Result, error) {
	progressf("[2/5] resolving rootfs snapshot metadata...")
	rootfsInfo, err := conchClient.SnapshotInfo(ctx, client.SnapshotInfoRequest{Key: rootfsSnapshotID, Namespace: namespace})
	if err != nil {
		return Result{}, fmt.Errorf("failed to get rootfs snapshot info for %s: %w", rootfsSnapshotID, err)
	}
	progressf("[3/5] collecting rootfs snapshot chain...")
	rootfsChain, err := conchClient.SnapshotChain(ctx, client.SnapshotInfoRequest{Key: rootfsSnapshotID, Namespace: namespace})
	if err != nil {
		return Result{}, fmt.Errorf("resolve rootfs snapshot chain: %w", err)
	}
	rootfsChainPaths, err := validateSnapshotChainPaths(rootfsChain.ChainPaths)
	if err != nil {
		return Result{}, fmt.Errorf("resolve rootfs snapshot chain: %w", err)
	}

	memName, vmName, err := resolveSnapshotComponentIDs(&rootfsInfo)
	if err != nil {
		return Result{}, err
	}

	progressf("[4/5] collecting mem/vm snapshot chains...")
	memChain, err := conchClient.SnapshotChain(ctx, client.SnapshotInfoRequest{Key: memName, Namespace: namespace})
	if err != nil {
		return Result{}, fmt.Errorf("resolve mem snapshot chain: %w", err)
	}
	memInfo := memChain.Info
	memChainPaths, err := validateSnapshotChainPaths(memChain.ChainPaths)
	if err != nil {
		return Result{}, fmt.Errorf("resolve mem snapshot chain: %w", err)
	}
	vmChain, err := conchClient.SnapshotChain(ctx, client.SnapshotInfoRequest{Key: vmName, Namespace: namespace})
	if err != nil {
		return Result{}, fmt.Errorf("resolve sandbox snapshot chain: %w", err)
	}
	vmInfo := vmChain.Info
	vmChainPaths, err := validateSnapshotChainPaths(vmChain.ChainPaths)
	if err != nil {
		return Result{}, fmt.Errorf("resolve sandbox snapshot chain: %w", err)
	}

	logrus.Debugf("Rootfs top Storage Path: %s", rootfsInfo.StoragePath)
	logrus.Debugf("Rootfs chain paths: %v", rootfsChainPaths)
	logrus.Debugf("Mem Storage Path: %s", memInfo.StoragePath)
	logrus.Debugf("Mem chain paths: %v", memChainPaths)
	logrus.Debugf("Sandbox Storage Path: %s", vmInfo.StoragePath)
	logrus.Debugf("Sandbox chain paths: %v", vmChainPaths)

	publisher := ocipublisher.NewSnapshotPublisher(store)
	if bootIndexTag == "" {
		bootIndexTag = "localhost/conch/sandbox-snapshot:latest"
	}

	progressf("[5/5] publishing sandbox-snapshot image %s...", bootIndexTag)
	bootIndexDigest, err := publisher.PublishSnapshotBundleFromPath(
		ctx,
		rootfsChainPaths,
		memChainPaths,
		vmChainPaths,
		bootIndexTag,
	)
	if err != nil {
		return Result{}, fmt.Errorf("failed to publish boot index from snapshot paths: %w", err)
	}

	logrus.Infof("[conch snapshot export] snapshot top keys: rootfs=%s sandbox=%s mem=%s", rootfsSnapshotID, vmName, memName)
	logrus.Infof("[conch snapshot export] boot index tag: %s", bootIndexTag)
	logrus.Infof("Final boot OCI index published: %s", bootIndexDigest.String())
	return Result{
		BootIndexDigest: bootIndexDigest.Encoded(),
		BootIndexTag:    bootIndexTag,
	}, nil
}

func resolveConchNamespace(configPath string) string {
	namespace := ""
	cfgPath := configPath
	if cfgPath == "" {
		cfgPath = config.FindConfigFile()
	}
	if cfg, err := config.LoadConfig(cfgPath); err == nil {
		if cfgPath != "" {
			logrus.Infof("Using config: %s", cfgPath)
		}
		namespace = strings.TrimSpace(cfg.Containerd.DefaultNamespace)
	}

	if namespace == "" {
		namespace = "default"
	}
	return namespace
}

func syncImageViaConchd(ctx context.Context, conchClient *client.Client, store storage.Store, imageID, imageRef, importedTag, namespace string, systemContext *types.SystemContext) (client.ImportImageResponse, error) {
	tmpDir, err := os.MkdirTemp("", "buildah-conchd-*")
	if err != nil {
		return client.ImportImageResponse{}, fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(tmpDir); rmErr != nil {
			logrus.Warnf("warning: failed to clean temp directory %s: %v", tmpDir, rmErr)
		}
	}()

	exportRef := imageRef
	if exportRef == "" {
		exportRef = imageID
	}
	if importedTag == "" {
		importedTag = "buildah-oci-image:latest"
	}
	tmpTarPath := filepath.Join(tmpDir, "image.tar")
	if err := export.ExportImageToTar(ctx, exportRef, tmpTarPath, "oci-archive", importedTag, systemContext, os.Stdout); err != nil {
		return client.ImportImageResponse{}, fmt.Errorf("failed to export image from storage to tar: %w", err)
	}

	return conchClient.ImportImageArchive(ctx, client.ImportImageRequest{
		ArchivePath: tmpTarPath,
		Namespace:   namespace,
		ImportedTag: importedTag,
	})
}

func validateSnapshotChainPaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("snapshot chain is empty")
	}
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			return nil, fmt.Errorf("snapshot chain contains empty storage path")
		}
	}
	return paths, nil
}

func collectSnapshotChainPathsWithGetter(topKey string, getInfo func(string) (*client.SnapshotMeta, error)) ([]string, error) {
	var rev []string
	cur := topKey
	seen := make(map[string]struct{})
	for cur != "" {
		if _, ok := seen[cur]; ok {
			return nil, fmt.Errorf("snapshot chain cycle detected at %s", cur)
		}
		seen[cur] = struct{}{}
		info, err := getInfo(cur)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", cur, err)
		}
		if strings.TrimSpace(info.StoragePath) == "" {
			return nil, fmt.Errorf("snapshot %s has empty storage path", cur)
		}
		rev = append(rev, info.StoragePath)
		cur = info.Parent
	}
	// reverse to parent-most -> top
	out := make([]string, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out, nil
}

func resolveSnapshotComponentIDs(rootfsInfo *client.SnapshotMeta) (string, string, error) {
	if rootfsInfo == nil {
		return "", "", fmt.Errorf("rootfs snapshot metadata is required")
	}

	memName, ok := rootfsInfo.Labels[SnapshotLabelMemSnapshot]
	if !ok || strings.TrimSpace(memName) == "" {
		return "", "", fmt.Errorf("rootfs snapshot missing mem association (label %s)", SnapshotLabelMemSnapshot)
	}

	vmName, ok := rootfsInfo.Labels[SnapshotLabelVMSnapshot]
	if !ok || strings.TrimSpace(vmName) == "" {
		return "", "", fmt.Errorf("rootfs snapshot missing sandbox association (label %s)", SnapshotLabelVMSnapshot)
	}

	return memName, vmName, nil
}
