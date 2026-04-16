package image

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/snapshots"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	SnapshotLabelMemSnapshot = "conch/snapshotter/mem-snapshot"
	SnapshotLabelVMSnapshot  = "conch/snapshotter/vm-snapshot"

	KindRootfs      = "rootfs"
	KindSandbox     = "sandbox"
	KindMemSnapshot = "mem-snapshot"
	KindUnknown     = "unknown"
)

// UnpackAllSubImages parses OCI Image Index, unpacks all child manifests,
// returns sub-image type (io.conch.kind) to snapshot ChainID mapping.
// On error, cleans up any snapshots already unpacked to avoid resource leakage.
//
// The image must be fully pulled before calling: all content (manifests, configs,
// layers) must exist in the content store. RootFS reads from the image config.
func UnpackAllSubImages(ctx context.Context, client *containerd.Client, imageName string) (snapshotMap map[string]string, err error) {
	snapshotMap = make(map[string]string)
	var createdSnapshotIDs []string
	snapshotter := client.SnapshotService("overlayfs")
	defer func() {
		if err != nil {
			cleanupSnapshots(createdSnapshotIDs, snapshotter, ctx)
		}
	}()

	index, err := getImageIndex(ctx, client, imageName)
	if err != nil {
		return nil, err
	}

	ulog.Info("Found manifests in index, starting unpack",
		ulog.F("count", len(index.Manifests)))

	for _, manifestDesc := range index.Manifests {
		kind := getKind(manifestDesc)
		snapshotID, err := unpackOneSubImage(ctx, client, snapshotter, manifestDesc, kind, &createdSnapshotIDs)
		if err != nil {
			return nil, err
		}
		snapshotMap[kind] = snapshotID
		ulog.Info("Generated SnapshotID",
			ulog.F("kind", kind),
			ulog.F("snapshot_id", snapshotID))
	}

	if err := validateRequiredKinds(snapshotMap); err != nil {
		return nil, err
	}

	if err = linkSnapshotLabels(ctx, snapshotter, snapshotMap); err != nil {
		return nil, err
	}
	return snapshotMap, nil
}

func cleanupSnapshots(createdSnapshotIDs []string, snapshotter snapshots.Snapshotter, ctx context.Context) {
	for _, sid := range createdSnapshotIDs {
		if removeErr := snapshotter.Remove(ctx, sid); removeErr != nil {
			ulog.Warn("Cleanup snapshot on error",
				ulog.F("snapshot_id", sid),
				ulog.F("error", removeErr))
		}
	}
}

func getImageIndex(ctx context.Context, client *containerd.Client, imageName string) (*ocispec.Index, error) {
	img, err := client.GetImage(ctx, imageName)
	if err != nil {
		return nil, fmt.Errorf("get image %s: %w", imageName, err)
	}

	target := img.Target()
	if target.MediaType != ocispec.MediaTypeImageIndex {
		return nil, fmt.Errorf("image %s is not an OCI Image Index (mediaType: %s)", imageName, target.MediaType)
	}

	indexData, err := content.ReadBlob(ctx, client.ContentStore(), target)
	if err != nil {
		return nil, fmt.Errorf("read index content: %w", err)
	}

	var index ocispec.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		return nil, fmt.Errorf("unmarshal index JSON: %w", err)
	}
	return &index, nil
}

// ValidateConchImageIndex verifies that the image is a Conch native OCI index
// containing at least rootfs and sandbox components.
func ValidateConchImageIndex(ctx context.Context, client *containerd.Client, imageName string) error {
	index, err := getImageIndex(ctx, client, imageName)
	if err != nil {
		return err
	}

	kinds := make(map[string]string, len(index.Manifests))
	for _, manifestDesc := range index.Manifests {
		kind := getKind(manifestDesc)
		if kind == KindUnknown {
			continue
		}
		kinds[kind] = manifestDesc.Digest.String()
	}

	return validateRequiredKinds(kinds)
}

func getKind(manifestDesc ocispec.Descriptor) string {
	if kind := manifestDesc.Annotations["io.conch.kind"]; kind != "" {
		return kind
	}
	return KindUnknown
}

func validateRequiredKinds(snapshotMap map[string]string) error {
	if snapshotMap[KindRootfs] == "" {
		return fmt.Errorf("boot index missing required kind %q", KindRootfs)
	}
	if snapshotMap[KindSandbox] == "" {
		return fmt.Errorf("boot index missing required kind %q", KindSandbox)
	}
	// KindMemSnapshot is optional for normal boot images and required only for snapshot images.
	return nil
}

func unpackOneSubImage(ctx context.Context, client *containerd.Client, snapshotter snapshots.Snapshotter, manifestDesc ocispec.Descriptor, kind string, createdSnapshotIDs *[]string) (string, error) {
	subImg := containerd.NewImage(client, images.Image{
		Name:   fmt.Sprintf("temp-unpack-%s", manifestDesc.Digest.Encoded()[:12]),
		Target: manifestDesc,
	})

	diffIDs, err := subImg.RootFS(ctx)
	if err != nil {
		return "", fmt.Errorf("get RootFS for %s: %w", kind, err)
	}
	snapshotID := identity.ChainID(diffIDs).String()

	if err := subImg.Unpack(ctx, "overlayfs"); err != nil {
		return "", fmt.Errorf("unpack sub-image %s (kind: %s): %w", manifestDesc.Digest, kind, err)
	}

	// Verify the snapshot was created with the expected ChainID (containerd uses this as the snapshot name)
	if _, err := snapshotter.Stat(ctx, snapshotID); err != nil {
		return "", fmt.Errorf("verify unpacked snapshot %s for %s: %w", snapshotID, kind, err)
	}
	*createdSnapshotIDs = append(*createdSnapshotIDs, snapshotID)
	return snapshotID, nil
}

func linkSnapshotLabels(ctx context.Context, snapshotter snapshots.Snapshotter, snapshotMap map[string]string) error {
	rootfsSID := snapshotMap[KindRootfs]
	sandboxSID := snapshotMap[KindSandbox]
	memSID := snapshotMap[KindMemSnapshot]
	if rootfsSID == "" || sandboxSID == "" {
		return fmt.Errorf("cannot link snapshot labels: need rootfs and sandbox kinds")
	}

	labels := make(map[string]string)
	fieldpaths := []string{}
	if sandboxSID != "" {
		labels[SnapshotLabelVMSnapshot] = sandboxSID
		fieldpaths = append(fieldpaths, "labels."+SnapshotLabelVMSnapshot)
	}
	if memSID != "" {
		labels[SnapshotLabelMemSnapshot] = memSID
		fieldpaths = append(fieldpaths, "labels."+SnapshotLabelMemSnapshot)
	}
	_, err := snapshotter.Update(ctx, snapshots.Info{
		Name:   rootfsSID,
		Labels: labels,
	}, fieldpaths...)
	if err != nil {
		return fmt.Errorf("failed to link component SnapshotIDs to rootfs: %w", err)
	}
	return nil
}
