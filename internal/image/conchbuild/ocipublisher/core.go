package ocipublisher

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/transports"
	imageStorage "go.podman.io/image/v5/storage"
	"go.podman.io/storage"
)

func CommitConchArtifact(
	store storage.Store,
	layer *storage.Layer,
	rawConfigJSON []byte,
	rawManifestJSON []byte,
	names []string, // Image names, e.g., ["conch-vm:snapshot-v1"]
) (*storage.Image, string, error) {

	// --- 1. Process Image Config ---
	configDigest := digest.FromBytes(rawConfigJSON)

	var normalizedNames []string
	for _, name := range names {
		// Automatically prepend "localhost/" and append ":latest" tag if missing
		ref, err := reference.ParseNormalizedNamed(name)
		if err != nil {
			return nil, "", fmt.Errorf("parsing name %q: %w", name, err)
		}
		ref = reference.TagNameOnly(ref)
		normalizedNames = append(normalizedNames, ref.String())
	}

	// --- 2. Parse and Update Manifest ---
	var m ConchManifest
	if err := json.Unmarshal(rawManifestJSON, &m); err != nil {
		return nil, "", fmt.Errorf("failed to parse manifest: %w", err)
	}

	// Sync layer info from the storage layer into the manifest descriptor
	if len(m.Layers) > 0 {
		m.Layers[0].Digest = layer.CompressedDigest
		m.Layers[0].Size = layer.CompressedSize
	}

	finalManifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal final manifest: %w", err)
	}
	finalManifestDigest := digest.FromBytes(finalManifestBytes)

	// --- 3. Prepare BigData ---
	// containers/storage requires Manifest and Config to be associated with the Image record as BigData blobs
	bigData := []storage.ImageBigDataOption{
		{
			Key:    configDigest.String(),
			Data:   rawConfigJSON,
			Digest: configDigest,
		},
		{
			Key:    storage.ImageDigestManifestBigDataNamePrefix + "-" + finalManifestDigest.String(),
			Data:   finalManifestBytes,
			Digest: finalManifestDigest,
		},
		// This specific key is mandatory for storage to recognize the image digest
		{
			Key:    storage.ImageDigestBigDataKey, // "manifest"
			Data:   finalManifestBytes,
			Digest: finalManifestDigest,
		},
	}

	// --- 4. Create Storage Image Record ---
	imageOptions := &storage.ImageOptions{
		BigData: bigData,
	}

	imageID := finalManifestDigest.Encoded()
	if err := claimImageNames(store, normalizedNames, imageID); err != nil {
		return nil, "", fmt.Errorf("failed to claim image names: %w", err)
	}

	img, err := store.CreateImage(imageID, normalizedNames, layer.ID, "", imageOptions)
	if err != nil {
		// Handle case where image ID already exists (e.g., re-publishing same content)
		if err == storage.ErrDuplicateID {
			img, _ = store.Image(imageID)
			_ = store.AddNames(img.ID, normalizedNames)
		} else {
			return nil, "", fmt.Errorf("failed to create image record: %w", err)
		}
	}

	// --- 5. Generate Standard Image Reference String ---
	// Result format: "containers-storage:[storage_dir]localhost/conch-vm:snapshot-v1"
	ref, err := imageStorage.Transport.ParseStoreReference(store, "@"+img.ID)
	if err != nil {
		return img, "", nil
	}

	return img, transports.ImageName(ref), nil
}

func processExternalBlob(data []byte) (digest.Digest, int64, error) {
	if len(data) == 0 {
		return "", 0, errors.New("input data is empty")
	}
	blobDigest := digest.FromBytes(data)
	blobSize := int64(len(data))
	return blobDigest, blobSize, nil
}
