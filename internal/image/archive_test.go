package image

import (
	"errors"
	"testing"

	"github.com/containerd/containerd/v2/core/images"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestSelectImportedSnapshotFallsBackForRegularOCIIndex(t *testing.T) {
	imported := []images.Image{{
		Name:   "example:latest",
		Target: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex},
	}}
	var regularUnpackCalls int

	snapshotKey, imageName, err := selectImportedSnapshot(imported,
		func(images.Image) (map[string]string, bool, error) {
			return nil, false, nil
		},
		func(imgInfo images.Image) (string, error) {
			regularUnpackCalls++
			if imgInfo.Name != "example:latest" {
				t.Fatalf("regular unpack image = %q, want example:latest", imgInfo.Name)
			}
			return "regular-snapshot", nil
		},
	)
	if err != nil {
		t.Fatalf("selectImportedSnapshot() error = %v", err)
	}
	if snapshotKey != "regular-snapshot" || imageName != "example:latest" {
		t.Fatalf("selectImportedSnapshot() = (%q, %q), want (regular-snapshot, example:latest)", snapshotKey, imageName)
	}
	if regularUnpackCalls != 1 {
		t.Fatalf("regular unpack calls = %d, want 1", regularUnpackCalls)
	}
}

func TestSelectImportedSnapshotReturnsConchIndexUnpackError(t *testing.T) {
	imported := []images.Image{{
		Name:   "conch:latest",
		Target: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex},
	}}
	unpackErr := errors.New("unpack conch index")

	_, _, err := selectImportedSnapshot(imported,
		func(images.Image) (map[string]string, bool, error) {
			return nil, true, unpackErr
		},
		func(images.Image) (string, error) {
			t.Fatal("regular unpack should not be called for a valid Conch index")
			return "", nil
		},
	)
	if !errors.Is(err, unpackErr) {
		t.Fatalf("selectImportedSnapshot() error = %v, want %v", err, unpackErr)
	}
}
