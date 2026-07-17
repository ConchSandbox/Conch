package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
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

// keyedUnpackLocks serializes unpack calls that target the same Boot Index
// (identified by key). A concurrent caller for an in-flight key blocks until
// the holder releases; a cancelled waiter returns without holding the lock.
type keyedUnpackLocks struct {
	mu      sync.Mutex
	entries map[string]chan struct{}
}

var bootIndexUnpackLocks keyedUnpackLocks

// acquire blocks until key is free and returns a release function, or returns
// ctx.Err() if the context is cancelled while waiting. The holder releases by
// closing the per-key done channel, which wakes all waiters to re-check.
func (locks *keyedUnpackLocks) acquire(ctx context.Context, key string) (func(), error) {
	for {
		locks.mu.Lock()
		if locks.entries == nil {
			locks.entries = make(map[string]chan struct{})
		}
		done, held := locks.entries[key]
		if !held {
			// Key is free: become the holder.
			done = make(chan struct{})
			locks.entries[key] = done
			locks.mu.Unlock()
			return func() {
				locks.mu.Lock()
				if current, ok := locks.entries[key]; ok && current == done {
					delete(locks.entries, key)
				}
				locks.mu.Unlock()
				close(done)
			}, nil
		}
		locks.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
			// Holder released; re-check whether the key is still held.
		}
	}
}

func bootIndexUnpackKey(ctx context.Context, client *containerd.Client, desc ocispec.Descriptor) string {
	namespace, ok := namespaces.Namespace(ctx)
	if !ok || namespace == "" {
		namespace = client.DefaultNamespace()
	}
	if namespace == "" {
		namespace = namespaces.Default
	}
	return namespace + "\x00" + desc.Digest.String()
}

// UnpackAllSubImages parses OCI Image Index, unpacks all child manifests,
// returns sub-image type (io.conch.kind) to snapshot ChainID mapping.
// On error, cleans up any snapshots already unpacked to avoid resource leakage.
//
// The image must be fully pulled before calling: all content (manifests, configs,
// layers) must exist in the content store. RootFS reads from the image config.
func UnpackAllSubImages(ctx context.Context, client *containerd.Client, imageName string) (snapshotMap map[string]string, err error) {
	desc, err := getBootIndexDescriptor(ctx, client, imageName)
	if err != nil {
		return nil, err
	}
	return unpackAllSubImagesFromDescriptor(ctx, client, desc, nil)
}

// UnpackAllSubImagesFromDescriptor unpacks a Boot Index directly from its
// immutable descriptor. It shares the exact validation and unpack path used by
// the image-name entrypoint.
func UnpackAllSubImagesFromDescriptor(ctx context.Context, client *containerd.Client, desc ocispec.Descriptor) (snapshotMap map[string]string, err error) {
	return unpackAllSubImagesFromDescriptor(ctx, client, desc, nil)
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
	manifestDesc, err := firstManifestDescriptorFromContent(ctx, client.ContentStore(), img.Target())
	if err != nil {
		return nil, fmt.Errorf("resolve default sandbox image %s manifest: %w", defaultSandboxImage, err)
	}
	desc := defaultSandboxDescriptor(manifestDesc, defaultSandboxImage)
	indexDesc, err := getBootIndexDescriptor(ctx, client, imageName)
	if err != nil {
		return nil, err
	}
	return unpackAllSubImagesFromDescriptor(ctx, client, indexDesc, &desc)
}

func unpackAllSubImagesFromDescriptor(ctx context.Context, client *containerd.Client, indexDesc ocispec.Descriptor, defaultSandbox *ocispec.Descriptor) (snapshotMap map[string]string, err error) {
	if client == nil {
		return nil, fmt.Errorf("containerd client is required")
	}
	release, err := bootIndexUnpackLocks.acquire(ctx, bootIndexUnpackKey(ctx, client, indexDesc))
	if err != nil {
		return nil, fmt.Errorf("wait to unpack boot index %s: %w", indexDesc.Digest, err)
	}
	defer release()

	snapshotMap = make(map[string]string)
	var createdSnapshots []createdSnapshot
	erofsSnapshotter := client.SnapshotService("erofs")
	defer func() {
		if err != nil {
			cleanupSnapshots(createdSnapshots, ctx)
		}
	}()

	index, err := readBootIndex(ctx, client, indexDesc)
	if err != nil {
		return nil, err
	}
	manifests := manifestsWithDefaultSandbox(index.Manifests, defaultSandbox)
	if _, err := validateBootIndexManifestKinds(manifests); err != nil {
		return nil, err
	}
	if err := validateContentClosure(ctx, client.ContentStore(), indexDesc); err != nil {
		return nil, fmt.Errorf("validate boot index %s closure: %w", indexDesc.Digest, err)
	}
	if defaultSandbox != nil {
		if err := validateContentClosure(ctx, client.ContentStore(), *defaultSandbox); err != nil {
			return nil, fmt.Errorf("validate default sandbox closure: %w", err)
		}
	}

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

	if err = recordRootfsSnapshotProvenance(ctx, erofsSnapshotter, snapshotMap, rootfsImageName, rootfsManifestDigest); err != nil {
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

func getBootIndexDescriptor(ctx context.Context, client *containerd.Client, imageName string) (ocispec.Descriptor, error) {
	if client == nil {
		return ocispec.Descriptor{}, fmt.Errorf("containerd client is required")
	}
	img, err := client.GetImage(ctx, imageName)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("get image %s: %w", imageName, err)
	}

	target := img.Target()
	if target.MediaType != ocispec.MediaTypeImageIndex {
		return ocispec.Descriptor{}, fmt.Errorf("image %s is not an OCI Image Index (mediaType: %s)", imageName, target.MediaType)
	}
	return target, nil
}

func readBootIndex(ctx context.Context, client *containerd.Client, desc ocispec.Descriptor) (*ocispec.Index, error) {
	if desc.MediaType != ocispec.MediaTypeImageIndex {
		return nil, fmt.Errorf("descriptor %s is not an OCI Image Index (mediaType: %s)", desc.Digest, desc.MediaType)
	}
	if err := validateDescriptor(desc, "boot index"); err != nil {
		return nil, err
	}
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

// ValidateBootIndexContent verifies that the image named by imageName is a
// valid Conch Boot Index: a content closure carrying the required rootfs and
// sandbox components. It performs static content checks only and does not boot
// a VM.
func ValidateBootIndexContent(ctx context.Context, client *containerd.Client, imageName string) error {
	desc, err := getBootIndexDescriptor(ctx, client, imageName)
	if err != nil {
		return err
	}
	_, err = InspectBootIndexContent(ctx, client.ContentStore(), desc)
	return err
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
	if err := ensureSnapshotChainUnpacked(ctx, snapshotter, diffIDs, func() error {
		return subImg.Unpack(ctx, snapshotterName)
	}, createdSnapshots); err != nil {
		return "", fmt.Errorf("unpack sub-image %s (kind: %s): %w", manifestDesc.Digest, kind, err)
	}
	return snapshotID, nil
}

func ensureSnapshotChainUnpacked(
	ctx context.Context,
	snapshotter snapshots.Snapshotter,
	diffIDs []digest.Digest,
	unpack func() error,
	createdSnapshots *[]createdSnapshot,
) error {
	if len(diffIDs) == 0 {
		return fmt.Errorf("component rootfs has no diff IDs")
	}
	chainIDs := make([]string, len(diffIDs))
	parents := make([]string, len(diffIDs))
	for i := range diffIDs {
		chainIDs[i] = identity.ChainID(diffIDs[:i+1]).String()
		if i > 0 {
			parents[i] = chainIDs[i-1]
		}
	}

	firstBad := -1
	for i, snapshotID := range chainIDs {
		health, err := inspectCommittedSnapshot(ctx, snapshotter, snapshotID, parents[i])
		switch health {
		case snapshotHealthy:
			if err != nil {
				return err
			}
		case snapshotMissing:
			if err != nil {
				return err
			}
			firstBad = i
		case snapshotCorrupt:
			firstBad = i
		default:
			return err
		}
		if firstBad >= 0 {
			break
		}
	}
	if firstBad < 0 {
		return nil
	}

	// A parent cannot be removed while descendants exist. Drop only the
	// unhealthy suffix from child to parent, preserving the verified prefix.
	for i := len(chainIDs) - 1; i >= firstBad; i-- {
		info, err := snapshotter.Stat(ctx, chainIDs[i])
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("stat snapshot %s for chain rebuild: %w", chainIDs[i], err)
		}
		if info.Kind != snapshots.KindCommitted {
			return fmt.Errorf("snapshot %s has kind %s, want committed", chainIDs[i], info.Kind)
		}
		if err := snapshotter.Remove(ctx, chainIDs[i]); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("remove snapshot %s for chain rebuild: %w", chainIDs[i], err)
		}
	}

	if err := unpack(); err != nil {
		recordExistingSnapshotSuffix(ctx, snapshotter, chainIDs, firstBad, createdSnapshots)
		return err
	}
	for i := len(chainIDs) - 1; i >= firstBad; i-- {
		recordCreatedSnapshot(createdSnapshots, snapshotter, chainIDs[i], false)
	}

	for i, snapshotID := range chainIDs {
		health, err := inspectCommittedSnapshot(ctx, snapshotter, snapshotID, parents[i])
		if err != nil {
			return fmt.Errorf("verify unpacked snapshot %s: %w", snapshotID, err)
		}
		if health != snapshotHealthy {
			return fmt.Errorf("verify unpacked snapshot %s: snapshot is not usable", snapshotID)
		}
	}
	return nil
}

func recordExistingSnapshotSuffix(
	ctx context.Context,
	snapshotter snapshots.Snapshotter,
	chainIDs []string,
	first int,
	createdSnapshots *[]createdSnapshot,
) {
	for i := len(chainIDs) - 1; i >= first; i-- {
		exists, err := snapshotExists(ctx, snapshotter, chainIDs[i])
		if err == nil && exists {
			recordCreatedSnapshot(createdSnapshots, snapshotter, chainIDs[i], false)
		}
	}
}

type snapshotHealth uint8

const (
	snapshotMissing snapshotHealth = iota
	snapshotHealthy
	snapshotCorrupt
)

// inspectCommittedSnapshot reports whether the committed snapshot identified by
// snapshotID is present and structurally healthy (committed kind, expected
// parent chain). It does not mount the backing data; content closure is
// validated separately by the caller. A missing snapshot is reported as
// snapshotMissing rather than an error so the chain rebuild can proceed.
func inspectCommittedSnapshot(ctx context.Context, snapshotter snapshots.Snapshotter, snapshotID, expectedParent string) (snapshotHealth, error) {
	info, err := snapshotter.Stat(ctx, snapshotID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return snapshotMissing, nil
		}
		return snapshotMissing, fmt.Errorf("stat snapshot %s: %w", snapshotID, err)
	}
	if info.Kind != snapshots.KindCommitted {
		return snapshotCorrupt, fmt.Errorf("snapshot %s has kind %s, want committed", snapshotID, info.Kind)
	}
	if info.Parent != expectedParent {
		return snapshotCorrupt, fmt.Errorf("snapshot %s has parent %q, want %q", snapshotID, info.Parent, expectedParent)
	}
	return snapshotHealthy, nil
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
	if preExisting || createdSnapshots == nil {
		return
	}
	*createdSnapshots = append(*createdSnapshots, createdSnapshot{key: snapshotID, snapshotter: snapshotter})
}

func recordRootfsSnapshotProvenance(ctx context.Context, snapshotter snapshots.Snapshotter, snapshotMap map[string]string, rootfsImageName, rootfsManifestDigest string) error {
	rootfsSID := snapshotMap[KindRootfs]
	if rootfsSID == "" {
		return fmt.Errorf("cannot link snapshot labels: rootfs snapshot is required")
	}

	labels := make(map[string]string)
	fieldpaths := []string{}
	if rootfsImageName != "" {
		labels[common.SnapshotLabelRootfsImage] = rootfsImageName
		fieldpaths = append(fieldpaths, "labels."+common.SnapshotLabelRootfsImage)
	}
	if rootfsManifestDigest != "" {
		labels[common.SnapshotLabelRootfsManifest] = rootfsManifestDigest
		fieldpaths = append(fieldpaths, "labels."+common.SnapshotLabelRootfsManifest)
	}
	if len(labels) == 0 {
		return nil
	}
	_, err := snapshotter.Update(ctx, snapshots.Info{
		Name:   rootfsSID,
		Labels: labels,
	}, fieldpaths...)
	if err != nil {
		return fmt.Errorf("failed to record rootfs snapshot provenance: %w", err)
	}
	return nil
}
