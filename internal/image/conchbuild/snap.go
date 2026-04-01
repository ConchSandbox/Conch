package conchbuild

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/snapshots"
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
	ConchAPIBaseURL string // optional; overrides BUILDAH_CONCH_API_URL / CONCHD_* when non-empty
}

// Result holds SNAP flow outputs needed by the caller.
type Result struct {
	BootIndexDigest string
	BootIndexTag    string
	PmemRootfsRef   string
}

// ExecuteSNAP runs the full SNAP flow: sync to containerd, call Conch Create/Pause,
// resolve rootfs/mem/vm paths, and publish a sandbox snapshot image. Returns the new imageID (index digest).
func ExecuteSNAP(ctx context.Context, opts SNAPOpts) (result Result, err error) {
	if len(opts.KernelArgs) != 2 {
		return Result{}, fmt.Errorf("SNAP instruction requires KERNEL instruction; add KERNEL <kernel_file> <initrd_file> before SNAP (e.g. KERNEL vmlinuz conch.initrd)")
	}
	kernelPath, _, err := client.ResolveKernelPaths(opts.ContextDir, opts.KernelArgs[0], opts.KernelArgs[1])
	if err != nil {
		return Result{}, fmt.Errorf("resolving kernel paths: %w", err)
	}

	containerdAddr := os.Getenv("CONTAINERD_ADDRESS")
	if containerdAddr == "" {
		containerdAddr = "/run/containerd/containerd.sock"
	}
	ctrdClient, err := containerd.New(containerdAddr)
	if err != nil {
		return Result{}, fmt.Errorf("failed to connect to containerd: %w", err)
	}
	defer ctrdClient.Close()

	snapCtx := namespaces.WithNamespace(ctx, "default")

	// Always convert OCI rootfs to EROFS and use it as VM DiskPath.
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

	var diskPathForVM string
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
		// Keep existing behavior: pass first layer as VM disk path.
		diskPathForVM = layers[0]
		logrus.Infof("[conch build] EROFS disk path: %s", diskPathForVM)
	} else {
		logrus.Infof("[conch build] EROFS mode: direct (output=%s)", erofsOut)
		// Direct: mount merged rootfs and run mkfs.erofs on directory.
		destPath, err := erofs.ConvertImageToEROFSDirect(ctx, opts.Store, opts.ImageID, opts.ImageRef, opts.SystemContext, erofsOut)
		if err != nil {
			return Result{}, fmt.Errorf("OCI→EROFS conversion failed: %w", err)
		}
		logrus.Infof("OCI→EROFS conversion complete: %s", destPath)
		rootfsLayers = append(rootfsLayers, destPath)
		diskPathForVM = destPath
		logrus.Infof("[conch build] EROFS disk path: %s", diskPathForVM)
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

	conchAPI := opts.ConchAPIBaseURL
	if conchAPI == "" {
		conchAPI = client.ResolveBaseURL()
	}
	conchClient := client.NewClient(conchAPI)
	sandboxID := client.GenSandboxID()

	if err := conchClient.CreateSandbox(rootfsImageName, sandboxID, kernelPath, diskPathForVM, client.DefaultRamMB); err != nil {
		return Result{}, fmt.Errorf("Conch CreateSandbox failed: %w", err)
	}
	logrus.Infof("Conch sandbox %s created, VM started", sandboxID)

	rootfsCommitName, err := conchClient.PauseSandbox(sandboxID)
	if err != nil {
		return Result{}, fmt.Errorf("Conch PauseSandbox failed: %w", err)
	}
	logrus.Infof("Conch Pause complete. Rootfs snapshot: %s", rootfsCommitName)
	logrus.Infof("[conch build] conch pause rootfs snapshot: %s", rootfsCommitName)

	rootfsInfo, err := sn.GetSnapshotInfo(snapCtx, ctrdClient, rootfsCommitName)
	if err != nil {
		return Result{}, fmt.Errorf("failed to get rootfs snapshot info for %s: %w", rootfsCommitName, err)
	}
	rootfsChainPaths, err := collectSnapshotChainPaths(snapCtx, ctrdClient, rootfsCommitName)
	if err != nil {
		return Result{}, fmt.Errorf("resolve rootfs snapshot chain: %w", err)
	}

	memName, ok := rootfsInfo.Labels[SnapshotLabelMemSnapshot]
	if !ok {
		return Result{}, fmt.Errorf("rootfs snapshot missing mem association (label %s)", SnapshotLabelMemSnapshot)
	}

	memInfo, err := sn.GetSnapshotInfo(snapCtx, ctrdClient, memName)
	if err != nil {
		return Result{}, fmt.Errorf("failed to get mem snapshot info for %s: %w", memName, err)
	}
	memChainPaths, err := collectSnapshotChainPaths(snapCtx, ctrdClient, memName)
	if err != nil {
		return Result{}, fmt.Errorf("resolve mem snapshot chain: %w", err)
	}
	vmName, ok := rootfsInfo.Labels[SnapshotLabelVMSnapshot]
	if !ok {
		return Result{}, fmt.Errorf("rootfs snapshot missing sandbox association (label %s)", SnapshotLabelVMSnapshot)
	}
	vmInfo, err := sn.GetSnapshotInfo(snapCtx, ctrdClient, vmName)
	if err != nil {
		return Result{}, fmt.Errorf("failed to get sandbox snapshot info for %s: %w", vmName, err)
	}
	vmChainPaths, err := collectSnapshotChainPaths(snapCtx, ctrdClient, vmName)
	if err != nil {
		return Result{}, fmt.Errorf("resolve sandbox snapshot chain: %w", err)
	}

	logrus.Debugf("Rootfs top Storage Path: %s", rootfsInfo.StoragePath)
	logrus.Debugf("Rootfs chain paths: %v", rootfsChainPaths)
	logrus.Debugf("Mem Storage Path: %s", memInfo.StoragePath)
	logrus.Debugf("Mem chain paths: %v", memChainPaths)
	logrus.Debugf("Sandbox Storage Path: %s", vmInfo.StoragePath)
	logrus.Debugf("Sandbox chain paths: %v", vmChainPaths)

	publisher := ocipublisher.NewSnapshotPublisher(opts.Store)
	bootIndexTag := opts.BootIndexTag
	if bootIndexTag == "" {
		bootIndexTag = "localhost/conch/sandbox-snapshot:latest"
	}

	bootIndexDigest, err := publisher.PublishSnapshotBundleFromPath(
		snapCtx,
		rootfsChainPaths,
		memChainPaths,
		vmChainPaths,
		bootIndexTag,
	)
	if err != nil {
		return Result{}, fmt.Errorf("failed to publish boot index from snapshot paths: %w", err)
	}

	logrus.Infof("[conch build] snapshot top keys: rootfs=%s sandbox=%s mem=%s", rootfsCommitName, vmName, memName)
	logrus.Infof("[conch build] boot index tag: %s", bootIndexTag)
	logrus.Infof("Final boot OCI index published: %s", bootIndexDigest.String())
	return Result{
		BootIndexDigest: bootIndexDigest.Encoded(),
		BootIndexTag:    bootIndexTag,
		PmemRootfsRef:   rootfsImageRef,
	}, nil
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
	var rev []string
	cur := topKey
	for cur != "" {
		info, err := sn.GetSnapshotInfo(ctx, ctrdClient, cur)
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
