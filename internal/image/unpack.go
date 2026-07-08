package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/openeuler/Conch/internal/image/erofsconvert"
	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	KindRootfs      = "rootfs"
	KindSandbox     = "sandbox"
	KindMemSnapshot = "mem-snapshot"
	KindUnknown     = "unknown"
)

var ErrMissingSandbox = errors.New("missing required sandbox component")

// UnpackAllSubImages parses OCI Image Index, unpacks all child manifests,
// returns sub-image type (io.conch.kind) to snapshot ChainID mapping.
// On error, cleans up any snapshots already unpacked to avoid resource leakage.
//
// The image must be fully pulled before calling: all content (manifests, configs,
// layers) must exist in the content store. RootFS reads from the image config.
func UnpackAllSubImages(ctx context.Context, client *containerd.Client, imageName string) (snapshotMap map[string]string, err error) {
	return unpackAllSubImages(ctx, client, imageName, nil)
}

// UnpackAllSubImagesWithDefaultSandbox unpacks a Conch boot index, using
// defaultSandboxImage as the sandbox component when the index only carries
// rootfs/mem-snapshot components.
func UnpackAllSubImagesWithDefaultSandbox(ctx context.Context, client *containerd.Client, imageName, defaultSandboxImage string) (map[string]string, error) {
	if defaultSandboxImage == "" {
		return UnpackAllSubImages(ctx, client, imageName)
	}
	img, err := client.GetImage(ctx, defaultSandboxImage)
	if err != nil {
		return nil, fmt.Errorf("get default sandbox image %s: %w", defaultSandboxImage, err)
	}
	desc := defaultSandboxDescriptor(img.Target(), defaultSandboxImage)
	return unpackAllSubImages(ctx, client, imageName, &desc)
}

func unpackAllSubImages(ctx context.Context, client *containerd.Client, imageName string, defaultSandbox *ocispec.Descriptor) (snapshotMap map[string]string, err error) {
	snapshotMap = make(map[string]string)
	var createdSnapshots []createdSnapshot
	erofsSnapshotter := client.SnapshotService("erofs")
	defer func() {
		if err != nil {
			cleanupSnapshots(createdSnapshots, ctx)
		}
	}()

	index, err := getImageIndex(ctx, client, imageName)
	if err != nil {
		return nil, err
	}
	manifests := manifestsWithDefaultSandbox(index.Manifests, defaultSandbox)

	ulog.Info("Found manifests in index, starting unpack",
		ulog.F("count", len(manifests)))

	rootfsImageName := ""
	rootfsManifestDigest := ""
	for _, manifestDesc := range manifests {
		kind := getKind(manifestDesc)
		snapshotter := erofsSnapshotter
		snapshotterName := "erofs"
		subImageName := componentImageName(kind, manifestDesc)
		if isNativeErofsKind(kind) {
			if err := validateNativeComponentManifest(ctx, client, kind, manifestDesc); err != nil {
				return nil, err
			}
			if err := ensureSubImage(ctx, client, subImageName, manifestDesc); err != nil {
				return nil, err
			}
			if kind == KindRootfs {
				rootfsImageName = subImageName
				rootfsManifestDigest = manifestDesc.Digest.String()
			}
		} else {
			return nil, fmt.Errorf("unsupported boot index component kind %q", kind)
		}
		snapshotID, err := unpackOneSubImage(ctx, client, snapshotter, snapshotterName, manifestDesc, kind, subImageName, &createdSnapshots)
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

	if err = linkSnapshotLabels(ctx, erofsSnapshotter, snapshotMap, rootfsImageName, rootfsManifestDigest); err != nil {
		return nil, err
	}
	return snapshotMap, nil
}

func manifestsWithDefaultSandbox(manifests []ocispec.Descriptor, defaultSandbox *ocispec.Descriptor) []ocispec.Descriptor {
	next := make([]ocispec.Descriptor, 0, len(manifests)+1)
	hasSandbox := false
	for _, manifest := range manifests {
		if getKind(manifest) == KindSandbox {
			hasSandbox = true
		}
		next = append(next, manifest)
	}
	if !hasSandbox && defaultSandbox != nil {
		next = append(next, *defaultSandbox)
	}
	return next
}

func defaultSandboxDescriptor(desc ocispec.Descriptor, imageName string) ocispec.Descriptor {
	desc.Annotations = mergeDescriptorAnnotations(desc.Annotations, map[string]string{
		"io.conch.kind":                     KindSandbox,
		"org.opencontainers.image.ref.name": imageName,
	})
	return desc
}

func mergeDescriptorAnnotations(base map[string]string, values map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(values))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range values {
		merged[k] = v
	}
	return merged
}

type createdSnapshot struct {
	key         string
	snapshotter snapshots.Snapshotter
}

func cleanupSnapshots(createdSnapshots []createdSnapshot, ctx context.Context) {
	for _, item := range createdSnapshots {
		if removeErr := item.snapshotter.Remove(ctx, item.key); removeErr != nil {
			ulog.Warn("Cleanup snapshot on error",
				ulog.F("snapshot_id", item.key),
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
	return readImageIndex(ctx, client, target)
}

func readImageIndex(ctx context.Context, client *containerd.Client, desc ocispec.Descriptor) (*ocispec.Index, error) {
	indexData, err := content.ReadBlob(ctx, client.ContentStore(), desc)
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
		return fmt.Errorf("boot index missing required kind %q: %w", KindSandbox, ErrMissingSandbox)
	}
	// KindMemSnapshot is optional for normal boot images and required only for snapshot images.
	return nil
}

func unpackOneSubImage(ctx context.Context, client *containerd.Client, snapshotter snapshots.Snapshotter, snapshotterName string, manifestDesc ocispec.Descriptor, kind string, imageName string, createdSnapshots *[]createdSnapshot) (string, error) {
	subImg := containerd.NewImage(client, images.Image{
		Name:   imageName,
		Target: manifestDesc,
	})

	diffIDs, err := subImg.RootFS(ctx)
	if err != nil {
		return "", fmt.Errorf("get RootFS for %s: %w", kind, err)
	}
	snapshotID := identity.ChainID(diffIDs).String()
	preExisting, err := snapshotExists(ctx, snapshotter, snapshotID)
	if err != nil {
		return "", fmt.Errorf("check snapshot %s before unpack for %s: %w", snapshotID, kind, err)
	}

	if err := subImg.Unpack(ctx, snapshotterName); err != nil {
		return "", fmt.Errorf("unpack sub-image %s (kind: %s): %w", manifestDesc.Digest, kind, err)
	}

	// Verify the snapshot was created with the expected ChainID (containerd uses this as the snapshot name)
	if _, err := snapshotter.Stat(ctx, snapshotID); err != nil {
		return "", fmt.Errorf("verify unpacked snapshot %s for %s: %w", snapshotID, kind, err)
	}
	recordCreatedSnapshot(createdSnapshots, snapshotter, snapshotID, preExisting)
	return snapshotID, nil
}

func validateNativeComponentManifest(ctx context.Context, client *containerd.Client, kind string, manifestDesc ocispec.Descriptor) error {
	manifest, err := images.Manifest(ctx, client.ContentStore(), manifestDesc, platforms.DefaultStrict())
	if err != nil {
		return fmt.Errorf("resolve native %s manifest: %w", kind, err)
	}
	if kind == KindRootfs {
		if _, err := erofsconvert.ValidateNativeLayers(manifest.Layers, erofsconvert.DefaultAlignBytes); err != nil {
			return fmt.Errorf("%s component is not native erofs: %w", kind, err)
		}
		return nil
	}
	if len(manifest.Layers) == 0 {
		return fmt.Errorf("%s component is not native erofs: manifest has no layers", kind)
	}
	for _, layer := range manifest.Layers {
		if layer.MediaType != erofsconvert.NativeLayerMediaType {
			return fmt.Errorf("%s component is not native erofs: layer %s media type %s is not %s", kind, layer.Digest, layer.MediaType, erofsconvert.NativeLayerMediaType)
		}
		if layer.Size <= 0 {
			return fmt.Errorf("%s component is not native erofs: layer %s size %d is invalid", kind, layer.Digest, layer.Size)
		}
	}
	return nil
}

func isNativeErofsKind(kind string) bool {
	return kind == KindRootfs || kind == KindMemSnapshot || kind == KindSandbox
}

func componentImageName(kind string, manifestDesc ocispec.Descriptor) string {
	return fmt.Sprintf("localhost/conch/%s-component:%s", kind, manifestDesc.Digest.Encoded())
}

func ensureSubImage(ctx context.Context, client *containerd.Client, imageName string, target ocispec.Descriptor) error {
	if imageName == "" {
		return fmt.Errorf("sub-image name is required")
	}
	_, err := client.ImageService().Create(ctx, images.Image{Name: imageName, Target: target})
	if err == nil || errdefs.IsAlreadyExists(err) {
		return nil
	}
	return fmt.Errorf("create sub-image record %s: %w", imageName, err)
}

func snapshotExists(ctx context.Context, snapshotter snapshots.Snapshotter, snapshotID string) (bool, error) {
	_, err := snapshotter.Stat(ctx, snapshotID)
	if err == nil {
		return true, nil
	}
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

func recordCreatedSnapshot(createdSnapshots *[]createdSnapshot, snapshotter snapshots.Snapshotter, snapshotID string, preExisting bool) {
	if preExisting {
		return
	}
	*createdSnapshots = append(*createdSnapshots, createdSnapshot{key: snapshotID, snapshotter: snapshotter})
}

func linkSnapshotLabels(ctx context.Context, snapshotter snapshots.Snapshotter, snapshotMap map[string]string, rootfsImageName, rootfsManifestDigest string) error {
	rootfsSID := snapshotMap[KindRootfs]
	sandboxSID := snapshotMap[KindSandbox]
	memSID := snapshotMap[KindMemSnapshot]
	if rootfsSID == "" || sandboxSID == "" {
		return fmt.Errorf("cannot link snapshot labels: need rootfs and sandbox kinds")
	}

	labels := make(map[string]string)
	fieldpaths := []string{}
	if sandboxSID != "" {
		labels[common.SnapshotLabelGroupVMRef] = sandboxSID
		fieldpaths = append(fieldpaths, "labels."+common.SnapshotLabelGroupVMRef)
	}
	if memSID != "" {
		labels[common.SnapshotLabelGroupMemRef] = memSID
		fieldpaths = append(fieldpaths, "labels."+common.SnapshotLabelGroupMemRef)
	}
	if rootfsImageName != "" {
		labels[common.SnapshotLabelRootfsImage] = rootfsImageName
		fieldpaths = append(fieldpaths, "labels."+common.SnapshotLabelRootfsImage)
	}
	if rootfsManifestDigest != "" {
		labels[common.SnapshotLabelRootfsManifest] = rootfsManifestDigest
		fieldpaths = append(fieldpaths, "labels."+common.SnapshotLabelRootfsManifest)
	}
	_, err := snapshotter.Update(ctx, snapshots.Info{
		Name:   rootfsSID,
		Labels: labels,
	}, fieldpaths...)
	if err != nil {
		return fmt.Errorf("failed to link component SnapshotIDs to rootfs: %w", err)
	}
	if err := updateComponentSnapshotLabels(ctx, snapshotter, sandboxSID, rootfsSID, common.SnapshotComponentKindVM); err != nil {
		return err
	}
	if memSID != "" {
		if err := updateComponentSnapshotLabels(ctx, snapshotter, memSID, rootfsSID, common.SnapshotComponentKindMem); err != nil {
			return err
		}
	}
	return nil
}

func updateComponentSnapshotLabels(ctx context.Context, snapshotter snapshots.Snapshotter, snapshotID, rootfsSID, kind string) error {
	labels := map[string]string{
		common.SnapshotLabelGroupID:       rootfsSID,
		common.SnapshotLabelComponentKind: kind,
	}
	fieldpaths := []string{
		"labels." + common.SnapshotLabelGroupID,
		"labels." + common.SnapshotLabelComponentKind,
	}
	_, err := snapshotter.Update(ctx, snapshots.Info{
		Name:   snapshotID,
		Labels: labels,
	}, fieldpaths...)
	if err != nil {
		return fmt.Errorf("failed to link component snapshot %s to rootfs %s: %w", snapshotID, rootfsSID, err)
	}
	return nil
}
