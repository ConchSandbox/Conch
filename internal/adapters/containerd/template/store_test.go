package containerdtemplate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/image/erofsconvert"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

var _ conchtemplate.Store = (*Store)(nil)

func TestStoreCRUDUsesCanonicalImageRecord(t *testing.T) {
	ctx := context.Background()
	contentStore := newTestContentStore(t)
	target := buildTestBootIndex(t, ctx, contentStore, false)
	imageStore := newMemoryImageStore()
	store := &Store{images: imageStore, content: contentStore}

	entry, err := store.Create(ctx, conchtemplate.Entry{
		Origin:          conchtemplate.OriginImage,
		BootMode:        conchtemplate.BootModeCold,
		BootIndexDigest: target.Digest.String(),
		SourceRef:       "registry.example/conch/demo:v1",
		Labels:          map[string]string{"owner": "team-a"},
	}, target)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	name, err := conchimage.CanonicalTemplateRef(target.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	record, err := imageStore.Get(ctx, name)
	if err != nil {
		t.Fatalf("Get canonical image record: %v", err)
	}
	if !reflect.DeepEqual(record.Target, target) {
		t.Fatalf("record target = %#v, want %#v", record.Target, target)
	}
	if got := record.Labels[schemaLabel]; got != schemaVersion {
		t.Fatalf("schema label = %q, want %q", got, schemaVersion)
	}
	if got := record.Labels[conchimage.ImageKindLabel]; got != conchimage.ImageKindBootIndexCold {
		t.Fatalf("image kind = %q", got)
	}
	if got := record.Labels[userLabelPrefix+"owner"]; got != "team-a" {
		t.Fatalf("user owner label = %q", got)
	}
	if entry.CreatedAt != record.CreatedAt.UnixNano() {
		t.Fatalf("entry CreatedAt = %d, want %d", entry.CreatedAt, record.CreatedAt.UnixNano())
	}

	contentInfo, err := contentStore.Info(ctx, target.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(contentInfo.Labels) == 0 {
		t.Fatal("Boot Index content has no containerd GC child labels")
	}

	imageStore.records["registry.example/ordinary:v1"] = images.Image{
		Name:   "registry.example/ordinary:v1",
		Target: target,
		Labels: map[string]string{conchimage.ImageKindLabel: conchimage.ImageKindOCIImage},
	}
	markerless, _ := conchimage.CanonicalTemplateRef(digest.FromString("markerless").String())
	imageStore.records[markerless] = images.Image{Name: markerless, Target: target}

	got, err := store.Get(ctx, target.Digest.String())
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.SourceRef != entry.SourceRef || got.Labels["owner"] != "team-a" {
		t.Fatalf("Get() = %#v, want source and user labels", got)
	}
	items, err := store.List(ctx, conchtemplate.Filter{
		Origin:   conchtemplate.OriginImage,
		BootMode: conchtemplate.BootModeCold,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 || items[0].BootIndexDigest != target.Digest.String() {
		t.Fatalf("List() = %#v, want only canonical Template", items)
	}

	if err := store.Delete(ctx, target.Digest.String()); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, target.Digest.String()); err != nil {
		t.Fatalf("idempotent Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, target.Digest.String()); !errors.Is(err, conchtemplate.ErrNotFound) {
		t.Fatalf("Get() after Delete error = %v, want ErrNotFound", err)
	}
}

func TestStoreCreateDoesNotOverwriteExistingTemplate(t *testing.T) {
	ctx := context.Background()
	contentStore := newTestContentStore(t)
	target := buildTestBootIndex(t, ctx, contentStore, false)
	store := &Store{images: newMemoryImageStore(), content: contentStore}

	first := conchtemplate.Entry{
		Origin:          conchtemplate.OriginImage,
		BootMode:        conchtemplate.BootModeCold,
		BootIndexDigest: target.Digest.String(),
		Labels:          map[string]string{"owner": "first"},
	}
	if _, err := store.Create(ctx, first, target); err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	first.Labels["owner"] = "second"
	if _, err := store.Create(ctx, first, target); !errors.Is(err, conchtemplate.ErrAlreadyExists) {
		t.Fatalf("second Create() error = %v, want ErrAlreadyExists", err)
	}
	got, err := store.Get(ctx, target.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["owner"] != "first" {
		t.Fatalf("existing Template was overwritten: %#v", got.Labels)
	}
}

func TestStoreCreateRejectsBootModeMismatch(t *testing.T) {
	ctx := context.Background()
	contentStore := newTestContentStore(t)
	target := buildTestBootIndex(t, ctx, contentStore, false)
	store := &Store{images: newMemoryImageStore(), content: contentStore}

	_, err := store.Create(ctx, conchtemplate.Entry{
		Origin:          conchtemplate.OriginCheckpoint,
		BootMode:        conchtemplate.BootModeResume,
		BootIndexDigest: target.Digest.String(),
	}, target)
	if !errors.Is(err, conchtemplate.ErrInvalidArtifact) {
		t.Fatalf("Create() error = %v, want ErrInvalidArtifact", err)
	}
}

type memoryImageStore struct {
	records map[string]images.Image
	filters []string
}

func newMemoryImageStore() *memoryImageStore {
	return &memoryImageStore{records: make(map[string]images.Image)}
}

func (s *memoryImageStore) Get(_ context.Context, name string) (images.Image, error) {
	record, ok := s.records[name]
	if !ok {
		return images.Image{}, errdefs.ErrNotFound
	}
	return record, nil
}

func (s *memoryImageStore) List(_ context.Context, filters ...string) ([]images.Image, error) {
	s.filters = append([]string(nil), filters...)
	out := make([]images.Image, 0, len(s.records))
	for _, record := range s.records {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *memoryImageStore) Create(_ context.Context, record images.Image) (images.Image, error) {
	if _, ok := s.records[record.Name]; ok {
		return images.Image{}, errdefs.ErrAlreadyExists
	}
	record.CreatedAt = time.Unix(10, 0).UTC()
	record.UpdatedAt = record.CreatedAt
	s.records[record.Name] = record
	return record, nil
}

func (s *memoryImageStore) Update(context.Context, images.Image, ...string) (images.Image, error) {
	return images.Image{}, errors.New("unexpected image update")
}

func (s *memoryImageStore) Delete(ctx context.Context, name string, opts ...images.DeleteOpt) error {
	record, ok := s.records[name]
	if !ok {
		return errdefs.ErrNotFound
	}
	var options images.DeleteOptions
	for _, opt := range opts {
		if err := opt(ctx, &options); err != nil {
			return err
		}
	}
	if options.Target != nil && record.Target.Digest != options.Target.Digest {
		return errdefs.ErrNotFound
	}
	delete(s.records, name)
	return nil
}

type memoryLabelStore struct {
	labels map[digest.Digest]map[string]string
}

func (s *memoryLabelStore) Get(dgst digest.Digest) (map[string]string, error) {
	return copyLabels(s.labels[dgst]), nil
}

func (s *memoryLabelStore) Set(dgst digest.Digest, labels map[string]string) error {
	s.labels[dgst] = copyLabels(labels)
	return nil
}

func (s *memoryLabelStore) Update(dgst digest.Digest, update map[string]string) (map[string]string, error) {
	labels := copyLabels(s.labels[dgst])
	if labels == nil {
		labels = make(map[string]string)
	}
	for key, value := range update {
		if value == "" {
			delete(labels, key)
		} else {
			labels[key] = value
		}
	}
	s.labels[dgst] = labels
	return copyLabels(labels), nil
}

func newTestContentStore(t *testing.T) content.Store {
	t.Helper()
	store, err := local.NewLabeledStore(t.TempDir(), &memoryLabelStore{
		labels: make(map[digest.Digest]map[string]string),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func buildTestBootIndex(t *testing.T, ctx context.Context, store content.Store, resume bool) ocispec.Descriptor {
	t.Helper()
	rootfs := writeTestComponent(t, ctx, store, conchimage.KindRootfs)
	sandbox := writeTestComponent(t, ctx, store, conchimage.KindSandbox)
	opts := conchimage.BootIndexContentOptions{
		RootfsDescriptor:  rootfs,
		SandboxDescriptor: sandbox,
	}
	if resume {
		opts.MemDescriptor = writeTestComponent(t, ctx, store, conchimage.KindMemSnapshot)
		opts.VMMName = "cloud-hypervisor"
		opts.MemorySizeMB = 128
	}
	target, err := conchimage.BuildBootIndexInContent(ctx, store, opts)
	if err != nil {
		t.Fatalf("BuildBootIndexInContent() error = %v", err)
	}
	return target
}

func writeTestComponent(t *testing.T, ctx context.Context, store content.Store, kind string) ocispec.Descriptor {
	t.Helper()
	layer := writeTestBlob(t, ctx, store, []byte("layer-"+kind), erofsconvert.NativeLayerMediaType)
	config := writeTestJSON(t, ctx, store, ocispec.Image{
		Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}},
	}, ocispec.MediaTypeImageConfig)
	manifest := writeTestJSON(t, ctx, store, ocispec.Manifest{
		Versioned: ispec.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    config,
		Layers:    []ocispec.Descriptor{layer},
	}, ocispec.MediaTypeImageManifest)
	manifest.Annotations = map[string]string{"io.conch.kind": kind}
	if kind == conchimage.KindMemSnapshot {
		manifest.Annotations[conchimage.AnnotationVMM] = "cloud-hypervisor"
		manifest.Annotations[conchimage.AnnotationMemorySizeMB] = "128"
	}
	return manifest
}

func writeTestJSON(t *testing.T, ctx context.Context, store content.Store, value any, mediaType string) ocispec.Descriptor {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return writeTestBlob(t, ctx, store, data, mediaType)
}

func writeTestBlob(t *testing.T, ctx context.Context, store content.Store, data []byte, mediaType string) ocispec.Descriptor {
	t.Helper()
	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}
	if err := content.WriteBlob(ctx, store, "test-"+desc.Digest.String(), bytes.NewReader(data), desc); err != nil {
		t.Fatal(err)
	}
	return desc
}

func copyLabels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
