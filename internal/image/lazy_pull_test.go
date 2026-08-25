package image

import (
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestParseLazyMemoryMetadataAllowsMissingProfile(t *testing.T) {
	manifest := ocispec.Manifest{
		Layers: []ocispec.Descriptor{{
			Size: 8192,
			Annotations: map[string]string{
				AnnotationMemoryFileOffset: "4096",
				AnnotationMemoryFileSize:   "2048",
			},
		}},
	}
	metadata, err := parseLazyMemoryMetadata("", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.FileOffset != 4096 || metadata.FileSize != 2048 {
		t.Fatalf("parsed metadata = %+v", metadata)
	}
	if len(metadata.Profile) != 0 {
		t.Fatalf("missing profile decoded into %d bytes", len(metadata.Profile))
	}
}

func TestParseLazyMemoryMetadataRejectsInvalidExtent(t *testing.T) {
	manifest := ocispec.Manifest{
		Layers: []ocispec.Descriptor{{
			Size: 4096,
			Annotations: map[string]string{
				AnnotationMemoryFileOffset: "4096",
				AnnotationMemoryFileSize:   "4097",
			},
		}},
	}
	if _, err := parseLazyMemoryMetadata("", manifest); err == nil {
		t.Fatal("memory file size beyond the EROFS layer was accepted")
	}
}
