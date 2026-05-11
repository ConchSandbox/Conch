package conchbuild

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/internal/image/conchbuild/client"
	"github.com/openeuler/Conch/internal/image/conchbuild/erofs"
	"github.com/openeuler/Conch/internal/image/conchbuild/ocipublisher"
	"github.com/openeuler/Conch/internal/image/conchbuild/rootfs"
	sn "github.com/openeuler/Conch/internal/image/conchbuild/snapshot"
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

	containerdAddr, containerdNamespace := resolveContainerdRuntime(opts.ConfigPath)
	ctrdClient, err := containerd.New(containerdAddr)
	if err != nil {
		return Result{}, fmt.Errorf("failed to connect to containerd: %w", err)
	}
	defer ctrdClient.Close()

	snapCtx := namespaces.WithNamespace(ctx, containerdNamespace)

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
			return Result{}, fmt.Errorf("OCI→EROFS conversion failed: %w", err)
		}
		if len(layers) == 0 {
			return Result{}, fmt.Errorf("OCI→EROFS conversion produced no layers")
		}
		logrus.Infof("OCI→EROFS conversion complete: %d layers in %s", len(layers), erofsOut)
		rootfsLayers = append(rootfsLayers, layers...)
		logrus.Infof("[conch build] EROFS disk path: %s", layers[0])
	} else {
		logrus.Infof("[conch build] EROFS mode: direct (output=%s)", erofsOut)
		// Direct: mount merged rootfs and run mkfs.erofs on directory.
		destPath, err := erofs.ConvertImageToEROFSDirect(ctx, opts.Store, opts.ImageID, opts.ImageRef, opts.SystemContext, erofsOut)
		if err != nil {
			return Result{}, fmt.Errorf("OCI→EROFS conversion failed: %w", err)
		}
		logrus.Infof("OCI→EROFS conversion complete: %s", destPath)
		rootfsLayers = append(rootfsLayers, destPath)
		logrus.Infof("[conch build] EROFS disk path: %s", destPath)
	}

	rootfsImageRef := "localhost/conch/pmem-rootfs:latest"
	if _, err := rootfs.BuildRootfsImage(ctx, opts.Store, rootfsLayers, rootfsImageRef); err != nil {
		return Result{}, fmt.Errorf("build pmem-rootfs image: %w", err)
	}

	rootfsSnapshotID, rootfsImageName, err := sn.SyncImageToContainerd(snapCtx, ctrdClient, opts.Store, "", rootfsImageRef, "buildah-oci-rootfs:latest", opts.SystemContext)
	if err != nil {
		return Result{}, fmt.Errorf("failed to sync image %s to containerd: %w", rootfsImageRef, err)
	}
	logrus.Infof("Successfully synced image %s to containerd (imageName=%s, snapshot=%s)", rootfsImageRef, rootfsImageName, rootfsSnapshotID)
	logrus.Infof("[conch build] containerd sync: image=%s snapshot=%s", rootfsImageName, rootfsSnapshotID)

	if opts.VMImageRef == "" {
		return Result{}, fmt.Errorf("kernel image ref is required for SNAP flow")
	}
	vmSnapshotID, _, err := sn.SyncImageToContainerd(snapCtx, ctrdClient, opts.Store, "", opts.VMImageRef, "buildah-oci-vm:latest", opts.SystemContext)
	if err != nil {
		return Result{}, fmt.Errorf("failed to sync kernel image %s to containerd: %w", opts.VMImageRef, err)
	}
	if err := linkRootfsSnapshotToVM(snapCtx, ctrdClient, rootfsSnapshotID, vmSnapshotID); err != nil {
		return Result{}, fmt.Errorf("link rootfs snapshot to sandbox snapshot: %w", err)
	}
	logrus.Infof("[conch build] linked rootfs snapshot %s -> sandbox snapshot %s", rootfsSnapshotID, vmSnapshotID)

	conchClient := client.NewClientWithConfig(opts.ConchAPIBaseURL, opts.ConfigPath)
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

	exported, err := exportSnapshotBundleFromRootfs(snapCtx, ctrdClient, opts.Store, rootfsCommitName, opts.BootIndexTag)
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

	containerdAddr, containerdNamespace := resolveContainerdRuntime(opts.ConfigPath)
	ctrdClient, err := containerd.New(containerdAddr)
	if err != nil {
		return Result{}, fmt.Errorf("failed to connect to containerd: %w", err)
	}
	defer ctrdClient.Close()

	snapCtx := namespaces.WithNamespace(ctx, containerdNamespace)
	rootfsSnapshotID := opts.RootfsSnapshotID
	if rootfsSnapshotID == "" {
		progressf("[1/5] pausing sandbox %s to create rootfs snapshot...", opts.SandboxID)
		conchClient := client.NewClientWithConfig(opts.ConchAPIBaseURL, opts.ConfigPath)
		rootfsSnapshotID, err = conchClient.PauseSandbox(opts.SandboxID, containerdNamespace)
		if err != nil {
			return Result{}, fmt.Errorf("Conch PauseSandbox failed: %w", err)
		}
		logrus.Infof("[conch snapshot export] conch pause rootfs snapshot: %s", rootfsSnapshotID)
	} else {
		progressf("[1/5] using existing rootfs snapshot %s...", rootfsSnapshotID)
	}

	return exportSnapshotBundleFromRootfs(snapCtx, ctrdClient, opts.Store, rootfsSnapshotID, opts.BootIndexTag)
}

func exportSnapshotBundleFromRootfs(ctx context.Context, ctrdClient *containerd.Client, store storage.Store, rootfsSnapshotID, bootIndexTag string) (Result, error) {
	progressf("[2/5] resolving rootfs snapshot metadata...")
	rootfsInfo, err := sn.GetSnapshotInfo(ctx, ctrdClient, rootfsSnapshotID)
	if err != nil {
		return Result{}, fmt.Errorf("failed to get rootfs snapshot info for %s: %w", rootfsSnapshotID, err)
	}
	progressf("[3/5] collecting rootfs snapshot chain...")
	rootfsChainPaths, err := collectSnapshotChainPaths(ctx, ctrdClient, rootfsSnapshotID)
	if err != nil {
		return Result{}, fmt.Errorf("resolve rootfs snapshot chain: %w", err)
	}

	memName, vmName, err := resolveSnapshotComponentIDs(rootfsInfo)
	if err != nil {
		return Result{}, err
	}

	progressf("[4/5] collecting mem/vm snapshot chains...")
	memInfo, err := sn.GetSnapshotInfo(ctx, ctrdClient, memName)
	if err != nil {
		return Result{}, fmt.Errorf("failed to get mem snapshot info for %s: %w", memName, err)
	}
	memChainPaths, err := collectSnapshotChainPaths(ctx, ctrdClient, memName)
	if err != nil {
		return Result{}, fmt.Errorf("resolve mem snapshot chain: %w", err)
	}
	vmInfo, err := sn.GetSnapshotInfo(ctx, ctrdClient, vmName)
	if err != nil {
		return Result{}, fmt.Errorf("failed to get sandbox snapshot info for %s: %w", vmName, err)
	}
	vmChainPaths, err := collectSnapshotChainPaths(ctx, ctrdClient, vmName)
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

func resolveContainerdRuntime(configPath string) (address string, namespace string) {
	address = strings.TrimSpace(os.Getenv("CONTAINERD_ADDRESS"))
	namespace = ""

	cfgPath := configPath
	if cfgPath == "" {
		cfgPath = config.FindConfigFile()
	}
	if cfg, err := config.LoadConfig(cfgPath); err == nil {
		if cfgPath != "" {
			logrus.Infof("Using config: %s", cfgPath)
		}
		if address == "" {
			address = strings.TrimSpace(cfg.Containerd.Socket)
		}
		namespace = strings.TrimSpace(cfg.Containerd.DefaultNamespace)
	}

	if address == "" {
		address = "/run/containerd/containerd.sock"
	}
	if namespace == "" {
		namespace = "default"
	}
	return address, namespace
}

func linkRootfsSnapshotToVM(ctx context.Context, ctrdClient *containerd.Client, rootfsSnapshotID, vmSnapshotID string) error {
	if rootfsSnapshotID == "" || vmSnapshotID == "" {
		return fmt.Errorf("rootfs and sandbox snapshot IDs are required")
	}
	snapshotter := ctrdClient.SnapshotService("overlayfs")
	_, err := snapshotter.Update(ctx, snapshots.Info{
		Name: rootfsSnapshotID,
		Labels: map[string]string{
			SnapshotLabelVMSnapshot: vmSnapshotID,
		},
	}, "labels."+SnapshotLabelVMSnapshot)
	if err != nil {
		return err
	}
	return nil
}

func collectSnapshotChainPaths(ctx context.Context, ctrdClient *containerd.Client, topKey string) ([]string, error) {
	return collectSnapshotChainPathsWithGetter(topKey, func(key string) (*sn.SnapshotMeta, error) {
		return sn.GetSnapshotInfo(ctx, ctrdClient, key)
	})
}

func collectSnapshotChainPathsWithGetter(topKey string, getInfo func(string) (*sn.SnapshotMeta, error)) ([]string, error) {
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
		if info.StoragePath == "" {
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

func resolveSnapshotComponentIDs(rootfsInfo *sn.SnapshotMeta) (string, string, error) {
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
