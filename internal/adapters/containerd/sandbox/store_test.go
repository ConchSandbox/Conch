package containerdsandbox

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/metadata"
	cdsandbox "github.com/containerd/containerd/v2/core/sandbox"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/runtimeapi"
	conchsandbox "github.com/openeuler/Conch/internal/sandbox"
)

var _ conchsandbox.Store = (*Store)(nil)

func TestStoreRoundTripAndPreservesForeignMetadata(t *testing.T) {
	ctx := context.Background()
	store, native, _, _ := newTestStore(t)
	internet := true
	want := conchsandbox.Record{
		ID:                       "sandbox-a",
		VMMPID:                   1234,
		State:                    conchsandbox.StateReady,
		SourceTemplateName:       "registry.example/conch/base:latest",
		SourceTemplateID:         "sha256:source",
		CheckpointHeadTemplateID: "sha256:head",
		IP:                       "10.12.0.2",
		VCPUNum:                  4,
		RamMB:                    2048,
		Network: &runtimeapi.SandboxNetworkConfig{
			AllowOut:            []string{"10.0.0.0/8"},
			AllowInternetAccess: &internet,
		},
		LastError: "",
		RuntimeSnapshots: []conchsandbox.SnapshotRef{
			{Snapshotter: "erofs", Role: "rootfs", Key: "rootfs-key"},
			{Snapshotter: "erofs", Role: "memory", Key: "memory-key"},
		},
	}

	created, err := store.Create(ctx, want)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.CreatedAt == 0 {
		t.Fatal("Create() did not return containerd creation time")
	}
	want.CreatedAt = created.CreatedAt

	nativeRecord, err := native.Get(containerdclient.NewNamespaceContext(ctx), want.ID)
	if err != nil {
		t.Fatalf("native Get() error = %v", err)
	}
	if nativeRecord.Sandboxer != sandboxerName {
		t.Fatalf("Sandboxer = %q, want %q", nativeRecord.Sandboxer, sandboxerName)
	}
	extension := nativeRecord.Extensions[extensionName]
	if extension == nil || extension.GetTypeUrl() != extensionName {
		t.Fatalf("metadata extension = %#v, want type %q", extension, extensionName)
	}
	if got := nativeRecord.Labels[gcSnapshotLabelPrefix+"erofs/rootfs"]; got != "rootfs-key" {
		t.Fatalf("rootfs GC label = %q", got)
	}

	nativeRecord.Labels["foreign.example/owner"] = "external"
	nativeRecord.Extensions["foreign.example/metadata"] = &types.Any{TypeUrl: "foreign", Value: []byte("data")}
	if _, err := native.Update(
		containerdclient.NewNamespaceContext(ctx),
		nativeRecord,
		"labels.foreign.example/owner",
		"extensions.foreign.example/metadata",
	); err != nil {
		t.Fatalf("native Update() error = %v", err)
	}

	want.State = conchsandbox.StateSuspended
	want.RuntimeSnapshots = []conchsandbox.SnapshotRef{
		{Snapshotter: "erofs", Role: "rootfs", Key: "rootfs-key-2"},
	}
	updated, err := store.Update(ctx, want)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !reflect.DeepEqual(updated, want) {
		t.Fatalf("Update() = %#v, want %#v", updated, want)
	}

	nativeRecord, err = native.Get(containerdclient.NewNamespaceContext(ctx), want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if nativeRecord.Labels["foreign.example/owner"] != "external" {
		t.Fatalf("foreign label was overwritten: %#v", nativeRecord.Labels)
	}
	if _, ok := nativeRecord.Extensions["foreign.example/metadata"]; !ok {
		t.Fatalf("foreign extension was overwritten: %#v", nativeRecord.Extensions)
	}
	if _, ok := nativeRecord.Labels[gcSnapshotLabelPrefix+"erofs/memory"]; ok {
		t.Fatalf("stale memory GC label remains: %#v", nativeRecord.Labels)
	}

	got, err := store.Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Get() = %#v, want %#v", got, want)
	}
	items, err := store.List(ctx, conchsandbox.Filter{State: conchsandbox.StateSuspended})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 || !reflect.DeepEqual(items[0], want) {
		t.Fatalf("List() = %#v, want %#v", items, []conchsandbox.Record{want})
	}
}

func TestStoreOwnershipErrorsAndIdempotentDelete(t *testing.T) {
	ctx := context.Background()
	store, native, _, _ := newTestStore(t)
	record := conchsandbox.Record{
		ID:               "sandbox-a",
		State:            conchsandbox.StateCreating,
		SourceTemplateID: "sha256:source",
	}
	if _, err := store.Create(ctx, record); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := store.Create(ctx, record); !errors.Is(err, conchsandbox.ErrAlreadyExists) {
		t.Fatalf("duplicate Create() error = %v, want ErrAlreadyExists", err)
	}

	foreignAny, err := typeurl.MarshalAny(&metadataV1{State: string(conchsandbox.StateReady)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.Create(containerdclient.NewNamespaceContext(ctx), cdsandbox.Sandbox{
		ID:         "foreign",
		Sandboxer:  "other",
		Extensions: map[string]typeurl.Any{extensionName: foreignAny},
	}); err != nil {
		t.Fatalf("create foreign Sandbox: %v", err)
	}
	if _, err := store.Get(ctx, "foreign"); !errors.Is(err, conchsandbox.ErrNotFound) {
		t.Fatalf("Get(foreign) error = %v, want ErrNotFound", err)
	}
	items, err := store.List(ctx, conchsandbox.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != record.ID {
		t.Fatalf("List() = %#v, want only Conch Sandbox", items)
	}

	if _, err := native.Create(containerdclient.NewNamespaceContext(ctx), cdsandbox.Sandbox{
		ID:        "malformed",
		Sandboxer: sandboxerName,
		Extensions: map[string]typeurl.Any{
			extensionName: &types.Any{TypeUrl: extensionName, Value: []byte("not-json")},
		},
	}); err != nil {
		t.Fatalf("create malformed Sandbox: %v", err)
	}
	if _, err := store.Get(ctx, "malformed"); !errors.Is(err, conchsandbox.ErrFailedPrecondition) {
		t.Fatalf("Get(malformed) error = %v, want ErrFailedPrecondition", err)
	}

	if err := store.Delete(ctx, record.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, record.ID); err != nil {
		t.Fatalf("idempotent Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, record.ID); !errors.Is(err, conchsandbox.ErrNotFound) {
		t.Fatalf("Get() after Delete error = %v, want ErrNotFound", err)
	}
}

func TestStoreAlwaysUsesConchNamespace(t *testing.T) {
	store, native, _, _ := newTestStore(t)
	callerCtx := namespaces.WithNamespace(context.Background(), "caller")
	if _, err := store.Create(callerCtx, conchsandbox.Record{
		ID:               "sandbox-a",
		State:            conchsandbox.StateCreating,
		SourceTemplateID: "sha256:source",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := native.Get(callerCtx, "sandbox-a"); err == nil {
		t.Fatal("Sandbox record unexpectedly exists in caller namespace")
	}
	if _, err := native.Get(containerdclient.NewNamespaceContext(context.Background()), "sandbox-a"); err != nil {
		t.Fatalf("Sandbox record is missing from Conch namespace: %v", err)
	}
}

func TestStoreBootIndexGCReferenceFollowsCurrentTemplateID(t *testing.T) {
	ctx := containerdclient.NewNamespaceContext(context.Background())
	store, _, db, contentStore := newTestStore(t)
	sourceChild := writeTestContent(t, ctx, contentStore, "source-child")
	source := writeTestContent(t, ctx, contentStore, "source")
	info, err := contentStore.Info(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	info.Labels = map[string]string{"containerd.io/gc.ref.content.child": sourceChild.String()}
	if _, err := contentStore.Update(ctx, info, "labels"); err != nil {
		t.Fatalf("label source content: %v", err)
	}

	record := conchsandbox.Record{
		ID:               "sandbox-a",
		State:            conchsandbox.StateCreating,
		SourceTemplateID: source.String(),
	}
	if _, err := store.Create(ctx, record); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := db.GarbageCollect(ctx); err != nil {
		t.Fatalf("GarbageCollect() error = %v", err)
	}
	assertContentExists(t, ctx, contentStore, source)
	assertContentExists(t, ctx, contentStore, sourceChild)

	head := writeTestContent(t, ctx, contentStore, "head")
	record.State = conchsandbox.StateReady
	record.CheckpointHeadTemplateID = head.String()
	if _, err := store.Update(ctx, record); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := db.GarbageCollect(ctx); err != nil {
		t.Fatalf("GarbageCollect() after update error = %v", err)
	}
	assertContentNotFound(t, ctx, contentStore, source)
	assertContentNotFound(t, ctx, contentStore, sourceChild)
	assertContentExists(t, ctx, contentStore, head)

	if err := store.Delete(ctx, record.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := db.GarbageCollect(ctx); err != nil {
		t.Fatalf("GarbageCollect() after delete error = %v", err)
	}
	assertContentNotFound(t, ctx, contentStore, head)
}

func newTestStore(t *testing.T) (*Store, cdsandbox.Store, *metadata.DB, content.Store) {
	t.Helper()
	bdb, err := bolt.Open(filepath.Join(t.TempDir(), "metadata.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := bdb.Close(); err != nil {
			t.Errorf("close metadata DB: %v", err)
		}
	})
	localStore, err := local.NewStore(filepath.Join(t.TempDir(), "content"))
	if err != nil {
		t.Fatal(err)
	}
	db := metadata.NewDB(bdb, localStore, nil)
	if err := db.Init(containerdclient.NewNamespaceContext(context.Background())); err != nil {
		t.Fatal(err)
	}
	native := metadata.NewSandboxStore(db)
	return NewStore(native), native, db, db.ContentStore()
}

func writeTestContent(t *testing.T, ctx context.Context, store content.Store, value string) digest.Digest {
	t.Helper()
	payload := []byte(value)
	dgst := digest.FromBytes(payload)
	desc := ocispec.Descriptor{Digest: dgst, Size: int64(len(payload))}
	if err := content.WriteBlob(ctx, store, "test-"+dgst.Encoded(), bytes.NewReader(payload), desc); err != nil {
		t.Fatalf("write content %s: %v", dgst, err)
	}
	return dgst
}

func assertContentExists(t *testing.T, ctx context.Context, store content.Store, dgst digest.Digest) {
	t.Helper()
	if _, err := store.Info(ctx, dgst); err != nil {
		t.Fatalf("content %s does not exist: %v", dgst, err)
	}
}

func assertContentNotFound(t *testing.T, ctx context.Context, store content.Store, dgst digest.Digest) {
	t.Helper()
	if _, err := store.Info(ctx, dgst); !errdefs.IsNotFound(err) {
		t.Fatalf("content %s error = %v, want not found", dgst, err)
	}
}
