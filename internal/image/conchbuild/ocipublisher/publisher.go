package ocipublisher

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"

	digest "github.com/opencontainers/go-digest"
	"github.com/openeuler/Conch/internal/image/conchbuild/buildahcli"
	"go.podman.io/storage"
)

// PublishSnapshotBundleFromPath orchestrates the end-to-end flow of packing
// snapshot directories into images and aggregating them into an OCI Index.
func (p *SnapshotPublisher) PublishSnapshotBundleFromPath(
	ctx context.Context,
	rootfsChainPaths []string,
	memChainPaths []string,
	vmChainPaths []string,
	bootIndexTag string,
) (digest.Digest, error) {

	// 1. Publish RootFS chain as a multi-layer OCI image.
	rootfsTag := bootIndexTag + "-rootfs-internal"
	rootfsImg, err := p.PublishComponentChain(ctx, rootfsChainPaths, rootfsTag, "rootfs")
	if err != nil {
		return "", fmt.Errorf("failed to publish rootfs component: %w", err)
	}

	// 2. Publish Memory Snapshot chain as a multi-layer OCI image.
	memTag := bootIndexTag + "-mem-internal"
	memImg, err := p.PublishComponentChain(ctx, memChainPaths, memTag, "mem-snapshot")
	if err != nil {
		return "", fmt.Errorf("failed to publish memory component: %w", err)
	}

	// 3. Publish VM Snapshot chain as a multi-layer OCI image.
	vmTag := bootIndexTag + "-vm-internal"
	vmImg, err := p.PublishComponentChain(ctx, vmChainPaths, vmTag, "sandbox")
	if err != nil {
		return "", fmt.Errorf("failed to publish vm component: %w", err)
	}

	// 4. Assemble components into the final boot OCI index using buildah's
	// native manifest commands so the result can be pushed with manifest push.
	return PublishBootIndex(
		ctx,
		p.store,
		rootfsImg.ID,
		rootfsTag,
		memImg,
		memTag,
		vmImg,
		vmTag,
		bootIndexTag,
	)
}

func runBuildah(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, buildahcli.Bin(), args...)
	cmd.Stderr = os.Stderr
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func ensureFreshManifestList(ctx context.Context, tag string) error {
	cmd := exec.CommandContext(ctx, buildahcli.Bin(), "manifest", "rm", tag)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		// Ignore absence; manifest rm returns non-zero when the list does not exist.
	}
	return runBuildah(ctx, "manifest", "create", tag)
}

func addManifestMember(ctx context.Context, indexTag, imageTag, kind, title string) error {
	return runBuildah(
		ctx,
		"manifest", "add",
		"--annotation", "io.conch.kind="+kind,
		"--annotation", "org.opencontainers.image.title="+title,
		indexTag,
		imageTag,
	)
}

// PublishBootIndex assembles rootfs/snapshot/vm into a final OCI index using
// buildah's native manifest implementation.
func PublishBootIndex(
	ctx context.Context,
	store storage.Store,
	rootfsID string,
	rootfsTag string,
	snapshotImg *storage.Image,
	snapshotTag string,
	vmImg *storage.Image,
	vmTag string,
	bootIndexTag string,
) (digest.Digest, error) {
	_ = rootfsID
	_ = snapshotImg
	_ = vmImg

	if err := claimImageNames(store, []string{bootIndexTag}, ""); err != nil {
		return "", fmt.Errorf("failed to claim boot index tag: %w", err)
	}
	if err := ensureFreshManifestList(ctx, bootIndexTag); err != nil {
		return "", fmt.Errorf("create boot manifest list: %w", err)
	}
	if err := addManifestMember(ctx, bootIndexTag, rootfsTag, "rootfs", "Base RootFS Image"); err != nil {
		return "", fmt.Errorf("add rootfs manifest: %w", err)
	}
	if err := addManifestMember(ctx, bootIndexTag, snapshotTag, "mem-snapshot", "Memory snapshot chain"); err != nil {
		return "", fmt.Errorf("add memory manifest: %w", err)
	}
	if err := addManifestMember(ctx, bootIndexTag, vmTag, "sandbox", "Sandbox Base Image"); err != nil {
		return "", fmt.Errorf("add sandbox manifest: %w", err)
	}

	img, err := store.Image(bootIndexTag)
	if err != nil {
		return "", fmt.Errorf("lookup boot index image %s: %w", bootIndexTag, err)
	}
	indexBytes, err := store.ImageBigData(img.ID, storage.ImageDigestBigDataKey)
	if err != nil {
		return "", fmt.Errorf("read boot index manifest: %w", err)
	}
	indexDigest := digest.FromBytes(indexBytes)

	fmt.Printf("Boot index created: %s (ID: %s)\n", bootIndexTag, img.ID)
	return indexDigest, nil
}
