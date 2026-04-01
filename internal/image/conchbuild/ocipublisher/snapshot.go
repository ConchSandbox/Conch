package ocipublisher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"time"

	digest "github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"go.podman.io/storage"
)

type SnapshotPublisher struct {
	store storage.Store
}

func NewSnapshotPublisher(store storage.Store) *SnapshotPublisher {
	return &SnapshotPublisher{store: store}
}

// PublishSingleComponent publishes a single directory path (Rootfs or Mem) as an independent OCI image.
func (p *SnapshotPublisher) PublishSingleComponent(ctx context.Context, srcPath, tag, componentType string) (*storage.Image, error) {
	// 1. Pack the physical directory into a tar.gz archive.
	// This uses the previously implemented createContainerLayerArchive function.
	archivePath, err := createContainerLayerArchive(srcPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create layer archive from %s: %w", srcPath, err)
	}
	defer os.Remove(archivePath)

	// 2. Write the archive into container storage as a new layer.
	layer, err := writeSnapshotLayer(p.store, archivePath)
	if err != nil {
		return nil, fmt.Errorf("failed to write snapshot layer: %w", err)
	}

	// 3. Dynamically generate an OCI Image Config for this specific component.
	// Note: Since this is a standalone image, DiffIDs will contain only a single element (the layer's UncompressedDigest).
	rawConfigJSON, err := GenerateSingleLayerConfig(layer.UncompressedDigest.String(), componentType)
	if err != nil {
		return nil, fmt.Errorf("failed to generate image config: %w", err)
	}

	// Calculate digest and size for the newly generated Config JSON blob.
	configDigest, configSize, err := processExternalBlob(rawConfigJSON)
	if err != nil {
		return nil, err
	}

	// 4. Construct the OCI Image Manifest for this component.
	rawManifestJSON, err := BuildSnapshotManifest(
		configDigest.String(),
		configSize,
		layer.CompressedDigest.String(),
		layer.CompressedSize,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build manifest: %w", err)
	}

	// 5. Commit the artifact to local storage and associate it with the provided tag.
	// This finalizes the image record in the containers/storage database.
	img, _, err := CommitConchArtifact(p.store, layer, rawConfigJSON, rawManifestJSON, []string{tag})
	return img, err
}

// PublishComponentChain publishes a snapshot chain as a multi-layer OCI image.
// Paths must be ordered from base(parent-most) to top(current snapshot).
func (p *SnapshotPublisher) PublishComponentChain(ctx context.Context, paths []string, tag, componentType string) (*storage.Image, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("empty snapshot chain")
	}

	type layerMeta struct {
		layer *storage.Layer
	}
	metas := make([]layerMeta, 0, len(paths))
	var parentLayerID string
	for _, pth := range paths {
		archivePath, err := createContainerLayerArchive(pth)
		if err != nil {
			return nil, fmt.Errorf("create layer archive from %s: %w", pth, err)
		}
		layer, err := writeSnapshotLayerWithParent(p.store, archivePath, parentLayerID)
		_ = os.Remove(archivePath)
		if err != nil {
			return nil, fmt.Errorf("write chain layer from %s: %w", pth, err)
		}
		parentLayerID = layer.ID
		metas = append(metas, layerMeta{layer: layer})
	}

	diffIDs := make([]string, 0, len(metas))
	now := time.Now()
	history := make([]v1.History, 0, len(metas))
	for range metas {
		history = append(history, v1.History{
			Created:    &now,
			CreatedBy:  "SnapshotPublisher chain " + componentType,
			EmptyLayer: false,
		})
	}
	for _, m := range metas {
		diffIDs = append(diffIDs, m.layer.UncompressedDigest.String())
	}
	rawConfigJSON, err := GenerateLayeredConfig(diffIDs, componentType, history)
	if err != nil {
		return nil, fmt.Errorf("generate multi-layer config: %w", err)
	}
	configDigest, configSize, err := processExternalBlob(rawConfigJSON)
	if err != nil {
		return nil, err
	}

	layerDigests := make([]string, 0, len(metas))
	layerSizes := make([]int64, 0, len(metas))
	for _, m := range metas {
		layerDigests = append(layerDigests, m.layer.CompressedDigest.String())
		layerSizes = append(layerSizes, m.layer.CompressedSize)
	}
	rawManifestJSON, err := BuildSnapshotManifestMulti(configDigest.String(), configSize, layerDigests, layerSizes)
	if err != nil {
		return nil, fmt.Errorf("build multi-layer manifest: %w", err)
	}

	// Top layer represents the image's top root.
	top := metas[len(metas)-1].layer
	img, _, err := CommitConchArtifact(p.store, top, rawConfigJSON, rawManifestJSON, []string{tag})
	return img, err
}

// PublishVMSnapshot is the main entry point for publishing.
// It receives a list of raw VM files and configuration, then generates a usable image in local storage.
// Specifically for cloud-hypervisor
func PublishVMSnapshot(ctx context.Context, store storage.Store, configPath string, dataFiles []string, tag string) (*storage.Image, error) {
	// --- 1. Archive data files (Tar.gz) ---
	logrus.Infof("Archiving data files: %v", dataFiles)
	archivePath, err := createVMSnapshotArchive(dataFiles)
	if err != nil {
		return nil, fmt.Errorf("failed to create archive: %w", err)
	}
	defer os.Remove(archivePath)

	// --- 2. Write data layer (PutLayer) ---
	layer, err := writeSnapshotLayer(store, archivePath)
	if err != nil {
		return nil, fmt.Errorf("failed to write layer: %w", err)
	}

	// --- 3. Process Config Blob ---
	logrus.Infof("Reading config file: %s", configPath)
	rawConfigJSON, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}
	configDigest, configSize, err := processExternalBlob(rawConfigJSON)
	if err != nil {
		return nil, err
	}

	// --- 4. Build initial Manifest (using external builder function) ---
	// Note: Construct an "intent" Manifest using the layer information
	logrus.Info("Building OCI Manifest...")
	rawManifestJSON, err := BuildSnapshotManifest(
		configDigest.String(),
		configSize,
		layer.CompressedDigest.String(),
		layer.CompressedSize,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build manifest: %w", err)
	}

	// --- 5. Finalize Commit (Commit Artifact) ---
	// This function parses the Manifest and ensures all fields are fully synchronized with the storage system
	logrus.Infof("Committing image record with tag: %s", tag)
	img, _, err := CommitConchArtifact(
		store,
		layer,
		rawConfigJSON,
		rawManifestJSON,
		[]string{tag},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to commit to storage: %w", err)
	}

	return img, nil
}

// writeSnapshotLayer writes the packaged tar.gz archive into the container storage as a new layer.
func writeSnapshotLayer(store storage.Store, archivePath string) (*storage.Layer, error) {
	return writeSnapshotLayerWithParent(store, archivePath, "")
}

func writeSnapshotLayerWithParent(store storage.Store, archivePath, parent string) (*storage.Layer, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open snapshot archive %s: %w", archivePath, err)
	}
	defer f.Close()

	fileInfo, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to get file info: %w", err)
	}
	layerSize := fileInfo.Size()

	// Generate a unique internal ID for the layer based on path and current timestamp
	newLayerID := digest.Canonical.FromString(archivePath + fmt.Sprintf("%d", time.Now().UnixNano())).Encoded()

	options := &storage.LayerOptions{
		// Tell PutLayer the original size to help it calculate digests and manage storage
		OriginalSize: &layerSize,
		// If the tarball is already compressed, OriginalDigest should be the compressed digest;
		// if uncompressed, PutLayer will automatically calculate the DiffID (UncompressedDigest).
	}

	// Core PutLayer call: Writes the io.Reader (f) as the layer content
	layer, _, err := store.PutLayer(
		newLayerID,
		parent,
		nil,
		"",
		false, // readonly: This is a read-only layer
		options,
		f, // Layer content reader
	)
	if err != nil {
		return nil, fmt.Errorf("failed to put VM snapshot layer: %w", err)
	}

	// Returns a Layer object containing DiffID, Digest, and Size information
	return layer, nil
}

// BuildSnapshotManifest constructs an OCI Manifest for the VM snapshot image.
func BuildSnapshotManifest(configDigest string, configSize int64, layerDigest string, layerSize int64) ([]byte, error) {
	manifest := v1.Manifest{
		Versioned: ispec.Versioned{
			SchemaVersion: 2,
		},

		MediaType: v1.MediaTypeImageManifest,

		// 1. Reference the Image Config Blob
		Config: v1.Descriptor{
			MediaType: v1.MediaTypeImageConfig,
			Digest:    digest.Digest(configDigest),
			Size:      configSize,
		},

		// 2. Reference the Data Layer Blob (the VM state tarball)
		Layers: []v1.Descriptor{
			{
				MediaType: VMDataLayerMediaType,
				Digest:    digest.Digest(layerDigest),
				Size:      layerSize,
				Annotations: map[string]string{
					"org.opencontainers.image.title": "Full State Data Layer V1",
				},
			},
		},

		// 3. Image-level Annotations
		Annotations: map[string]string{
			"org.opencontainers.image.title": "VM Full State Snapshot (V1 Base)",
			"io.conch.image.vm.state.type":   "full-state",
			// Note: Parent digest can be added here for incremental snapshots in the future
		},
	}

	return json.MarshalIndent(manifest, "", "  ")
}

// BuildSnapshotManifestMulti constructs a standard OCI image manifest for multiple layers.
func BuildSnapshotManifestMulti(configDigest string, configSize int64, layerDigests []string, layerSizes []int64) ([]byte, error) {
	if len(layerDigests) == 0 || len(layerDigests) != len(layerSizes) {
		return nil, fmt.Errorf("invalid layer metadata")
	}
	layers := make([]v1.Descriptor, 0, len(layerDigests))
	for i := range layerDigests {
		layers = append(layers, v1.Descriptor{
			MediaType: VMDataLayerMediaType,
			Digest:    digest.Digest(layerDigests[i]),
			Size:      layerSizes[i],
		})
	}
	manifest := v1.Manifest{
		Versioned: ispec.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest,
		Config: v1.Descriptor{
			MediaType: v1.MediaTypeImageConfig,
			Digest:    digest.Digest(configDigest),
			Size:      configSize,
		},
		Layers: layers,
	}
	return json.MarshalIndent(manifest, "", "  ")
}

func GenerateSingleLayerConfig(diffID string, componentType string) ([]byte, error) {
	now := time.Now()
	config := v1.Image{
		Created: &now,
		Platform: v1.Platform{
			Architecture: runtime.GOARCH,
			OS:           runtime.GOOS,
		},
		RootFS: v1.RootFS{
			Type: "layers",
			DiffIDs: []digest.Digest{
				digest.Digest(diffID),
			},
		},
		Config: v1.ImageConfig{
			Labels: map[string]string{
				"io.conch.component.type": componentType,
			},
		},
		History: []v1.History{
			{
				Created:    &now,
				CreatedBy:  "SnapshotPublisher: " + componentType,
				EmptyLayer: false,
			},
		},
	}

	return json.MarshalIndent(config, "", "  ")
}

func GenerateLayeredConfig(diffIDs []string, componentType string, history []v1.History) ([]byte, error) {
	now := time.Now()
	diffs := make([]digest.Digest, 0, len(diffIDs))
	for _, d := range diffIDs {
		diffs = append(diffs, digest.Digest(d))
	}
	config := v1.Image{
		Created: &now,
		Platform: v1.Platform{
			Architecture: runtime.GOARCH,
			OS:           runtime.GOOS,
		},
		RootFS: v1.RootFS{
			Type:    "layers",
			DiffIDs: diffs,
		},
		Config: v1.ImageConfig{
			Labels: map[string]string{
				"io.conch.component.type": componentType,
			},
		},
		History: history,
	}
	return json.MarshalIndent(config, "", "  ")
}
