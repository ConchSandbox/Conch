package image

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/openeuler/Conch/internal/image/erofsconvert"
)

func TestTemplateArchiveRoundTripPreservesBootIndexAndMetadata(t *testing.T) {
	ctx := context.Background()
	source := newArchiveContentStore(t)
	cold := buildArchiveBootIndex(t, ctx, source, false)
	resume := buildArchiveBootIndex(t, ctx, source, true)
	parent := digest.FromString("parent-boot-index").String()
	records := []TemplateArchiveRecord{
		{
			Name:            "localhost/conch/base:latest",
			Origin:          archiveOriginImage,
			BootIndexDigest: cold.Digest.String(),
			SourceRef:       "docker.io/library/busybox:latest",
			Labels:          map[string]string{"role": "base"},
		},
		{
			Name:                  "localhost/conch/checkpoint:latest",
			Origin:                archiveOriginCheckpoint,
			BootIndexDigest:       resume.Digest.String(),
			ParentBootIndexDigest: parent,
			SourceSandboxID:       "sandbox-1",
			Labels:                map[string]string{"owner": "team"},
		},
	}

	var raw bytes.Buffer
	if err := ExportTemplateArchive(ctx, source, &raw, records); err != nil {
		t.Fatalf("ExportTemplateArchive() error = %v", err)
	}
	var again bytes.Buffer
	if err := ExportTemplateArchive(ctx, source, &again, records); err != nil {
		t.Fatalf("second ExportTemplateArchive() error = %v", err)
	}
	if raw.Len() == 0 || raw.Len() != again.Len() {
		t.Fatalf("archive size = %d and %d, want equal non-zero sizes", raw.Len(), again.Len())
	}

	dest := newArchiveContentStore(t)
	loaded, err := ImportTemplateArchive(ctx, dest, bytes.NewReader(raw.Bytes()))
	if err != nil {
		t.Fatalf("ImportTemplateArchive() error = %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("loaded %d templates, want 2", len(loaded))
	}
	if loaded[0].Name != records[0].Name || loaded[0].Origin != archiveOriginImage || loaded[0].Resume ||
		loaded[0].BootIndexDigest != cold.Digest.String() || loaded[0].SourceRef != records[0].SourceRef ||
		loaded[0].Labels["role"] != "base" {
		t.Fatalf("cold record = %#v", loaded[0])
	}
	if loaded[1].Name != records[1].Name || loaded[1].Origin != archiveOriginCheckpoint || !loaded[1].Resume ||
		loaded[1].ParentBootIndexDigest != parent || loaded[1].SourceSandboxID != "sandbox-1" ||
		loaded[1].BootIndexDigest != resume.Digest.String() || loaded[1].Labels["owner"] != "team" {
		t.Fatalf("resume record = %#v", loaded[1])
	}
	for _, record := range loaded {
		info, err := InspectBootIndexContent(ctx, dest, record.Target)
		if err != nil {
			t.Fatalf("InspectBootIndexContent(%s) error = %v", record.Name, err)
		}
		if info.BootIndexDigest != record.BootIndexDigest {
			t.Fatalf("imported digest = %s, want %s", info.BootIndexDigest, record.BootIndexDigest)
		}
	}

	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := io.Copy(gz, bytes.NewReader(raw.Bytes())); err != nil {
		t.Fatalf("gzip archive: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	gzipped, err := ImportTemplateArchive(ctx, newArchiveContentStore(t), bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatalf("ImportTemplateArchive(gzip) error = %v", err)
	}
	if len(gzipped) != 2 || gzipped[0].BootIndexDigest != cold.Digest.String() {
		t.Fatalf("gzipped records = %#v", gzipped)
	}
}

func TestTemplateArchiveRejectsInvalidInputs(t *testing.T) {
	ctx := context.Background()
	store := newArchiveContentStore(t)
	cold := buildArchiveBootIndex(t, ctx, store, false)

	if _, err := ImportTemplateArchive(ctx, store, bytes.NewReader([]byte("not-an-archive"))); !errors.Is(err, ErrInvalidContent) {
		t.Fatalf("ImportTemplateArchive(garbage) error = %v, want invalid content", err)
	}
	if err := ExportTemplateArchive(ctx, store, io.Discard, []TemplateArchiveRecord{{
		Name:            "localhost/conch/missing:latest",
		Origin:          archiveOriginImage,
		BootIndexDigest: digest.FromString("missing").String(),
	}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ExportTemplateArchive(missing) error = %v, want not found", err)
	}
	if err := ExportTemplateArchive(ctx, store, io.Discard, []TemplateArchiveRecord{{
		Name:            "localhost/conch/base:latest",
		Origin:          archiveOriginCheckpoint,
		BootIndexDigest: cold.Digest.String(),
	}}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ExportTemplateArchive(origin) error = %v, want invalid argument", err)
	}
}

func newArchiveContentStore(t *testing.T) content.Store {
	t.Helper()
	store, err := localcontent.NewStore(filepath.Join(t.TempDir(), "content"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func buildArchiveBootIndex(t *testing.T, ctx context.Context, store content.Store, resume bool) ocispec.Descriptor {
	t.Helper()
	rootfs := writeArchiveComponent(t, ctx, store, KindRootfs)
	sandbox := writeArchiveComponent(t, ctx, store, KindSandbox)
	opts := BootIndexContentOptions{RootfsDescriptor: rootfs, SandboxDescriptor: sandbox}
	if resume {
		opts.MemDescriptor = writeArchiveComponent(t, ctx, store, KindMemSnapshot)
		opts.VMMName = "cloud-hypervisor"
		opts.MemorySizeMB = 128
		opts.CPUCount = 2
	}
	desc, err := BuildBootIndexInContent(ctx, store, opts)
	if err != nil {
		t.Fatalf("BuildBootIndexInContent(resume=%v) error = %v", resume, err)
	}
	return desc
}

func writeArchiveComponent(t *testing.T, ctx context.Context, store content.Store, kind string) ocispec.Descriptor {
	t.Helper()
	layer := writeArchiveBlob(t, ctx, store, []byte("layer-"+kind), erofsconvert.NativeLayerMediaType)
	config := writeArchiveJSON(t, ctx, store, ocispec.Image{
		Platform: ocispec.Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH},
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}},
	}, ocispec.MediaTypeImageConfig)
	manifest := writeArchiveJSON(t, ctx, store, ocispec.Manifest{
		Versioned: ispec.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    config,
		Layers:    []ocispec.Descriptor{layer},
	}, ocispec.MediaTypeImageManifest)
	manifest.Annotations = map[string]string{"io.conch.kind": kind}
	if kind == KindMemSnapshot {
		manifest.Annotations[AnnotationVMM] = "cloud-hypervisor"
		manifest.Annotations[AnnotationMemorySizeMB] = "128"
		manifest.Annotations[AnnotationCPUCount] = "2"
	}
	return manifest
}

func writeArchiveJSON(t *testing.T, ctx context.Context, store content.Store, value any, mediaType string) ocispec.Descriptor {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return writeArchiveBlob(t, ctx, store, data, mediaType)
}

func writeArchiveBlob(t *testing.T, ctx context.Context, store content.Store, data []byte, mediaType string) ocispec.Descriptor {
	t.Helper()
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	if err := content.WriteBlob(ctx, store, "archive-test-"+desc.Digest.String(), bytes.NewReader(data), desc); err != nil {
		t.Fatal(err)
	}
	return desc
}
