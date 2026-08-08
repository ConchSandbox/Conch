package image_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/metadata"
	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	conchimage "github.com/openeuler/Conch/internal/image"
)

func TestTemplateLeasesRetainImmutableIndexesAfterTagMovesAndGC(t *testing.T) {
	ctx := containerdclient.NewNamespaceContext(context.Background())
	client, db := newMetadataClient(t, ctx)
	store := client.ContentStore()
	tag := "localhost/conch/template:mutable"

	first := writeTestBootIndex(t, ctx, store, tag, "first")
	if _, err := client.ImageService().Create(ctx, images.Image{Name: tag, Target: first}); err != nil {
		t.Fatal(err)
	}
	if err := conchimage.RetainTemplateResources(ctx, client, "template-first", first.Digest.String()); err != nil {
		t.Fatal(err)
	}

	second := writeTestBootIndex(t, ctx, store, tag, "second")
	if _, err := client.ImageService().Update(ctx, images.Image{Name: tag, Target: second}, "target"); err != nil {
		t.Fatal(err)
	}
	if err := conchimage.RetainTemplateResources(ctx, client, "template-second", second.Digest.String()); err != nil {
		t.Fatal(err)
	}
	if err := client.ImageService().Delete(ctx, tag); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GarbageCollect(ctx); err != nil {
		t.Fatal(err)
	}

	for _, dgst := range []digest.Digest{first.Digest, second.Digest} {
		if _, err := conchimage.InspectBootIndex(ctx, client, dgst.String()); err != nil {
			t.Fatalf("boot index %s closure was collected: %v", dgst, err)
		}
	}

	if err := client.LeasesService().Delete(ctx, leases.Lease{ID: "conch.template.template-first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GarbageCollect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := conchimage.InspectBootIndex(ctx, client, first.Digest.String()); err == nil {
		t.Fatal("first boot index remained after its only Template lease was released")
	}
	if _, err := conchimage.InspectBootIndex(ctx, client, second.Digest.String()); err != nil {
		t.Fatalf("second boot index was damaged by first Template cleanup: %v", err)
	}
}

func newMetadataClient(t *testing.T, ctx context.Context) (*containerdclient.Client, *metadata.DB) {
	t.Helper()
	root := t.TempDir()
	localStore, err := localcontent.NewStore(filepath.Join(root, "content"))
	if err != nil {
		t.Fatal(err)
	}
	boltDB, err := bolt.Open(filepath.Join(root, "metadata.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = boltDB.Close() })
	db := metadata.NewDB(boltDB, localStore, nil)
	if err := db.Init(ctx); err != nil {
		t.Fatal(err)
	}
	rawClient, err := containerd.New("", containerd.WithServices(
		containerd.WithContentStore(db.ContentStore()),
		containerd.WithImageStore(metadata.NewImageStore(db)),
		containerd.WithLeasesService(metadata.NewLeaseManager(db)),
	))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rawClient.Close() })
	return &containerdclient.Client{Client: rawClient}, db
}

func writeTestBootIndex(t *testing.T, ctx context.Context, store content.Store, tag, marker string) ocispec.Descriptor {
	t.Helper()
	rootfs := writeTestComponent(t, ctx, store, conchimage.KindRootfs, marker+"-rootfs")
	sandbox := writeTestComponent(t, ctx, store, conchimage.KindSandbox, marker+"-sandbox")
	desc, err := conchimage.BuildBootIndexInContent(ctx, store, conchimage.BootIndexContentOptions{
		RootfsDescriptor: rootfs, SandboxDescriptor: sandbox, Tag: tag,
	})
	if err != nil {
		t.Fatal(err)
	}
	return desc
}

func writeTestComponent(t *testing.T, ctx context.Context, store content.Store, kind, marker string) ocispec.Descriptor {
	t.Helper()
	layer := writeTestBlob(t, ctx, store, []byte(marker), ocispec.MediaTypeImageLayer)
	config := ocispec.Image{
		RootFS: ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}},
		Config: ocispec.ImageConfig{Labels: map[string]string{"io.conch.component.type": kind}},
	}
	configDesc := writeTestJSON(t, ctx, store, config, ocispec.MediaTypeImageConfig)
	manifest := ocispec.Manifest{
		Versioned: ispec.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layer},
	}
	manifestDesc := writeTestJSON(t, ctx, store, manifest, ocispec.MediaTypeImageManifest)
	manifestDesc.Annotations = map[string]string{"io.conch.kind": kind}
	return manifestDesc
}

func writeTestJSON(t *testing.T, ctx context.Context, store content.Store, value any, mediaType string) ocispec.Descriptor {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return writeTestBlob(t, ctx, store, raw, mediaType)
}

func writeTestBlob(t *testing.T, ctx context.Context, store content.Store, raw []byte, mediaType string) ocispec.Descriptor {
	t.Helper()
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
	if err := content.WriteBlob(ctx, store, "test-"+desc.Digest.Encoded(), bytes.NewReader(raw), desc); err != nil {
		t.Fatal(err)
	}
	return desc
}
