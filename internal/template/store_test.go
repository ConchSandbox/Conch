package template

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/daemon/state"
)

func TestStoreCreateMarkReadyAndList(t *testing.T) {
	ctx := context.Background()
	raw := newTestStateStore(t)
	store := NewStore(raw)
	store.now = func() time.Time { return time.Unix(10, 0) }

	rec, err := store.Create(ctx, CreateRequest{
		Origin:    state.TemplateOriginImage,
		Namespace: "team-a",
		ImageName: "image-ref",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !strings.HasPrefix(rec.ID, "tmpl_") {
		t.Fatalf("id = %q, want tmpl_ prefix", rec.ID)
	}
	if rec.State != state.TemplateCreating {
		t.Fatalf("state = %q", rec.State)
	}
	if mode := BootMode(rec); mode != "" {
		t.Fatalf("creating template boot mode = %q, want empty", mode)
	}

	if err := store.MarkReady(ctx, rec.ID, Refs{RootfsKey: "rootfs", VMKey: "vm"}); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != state.TemplateReady || got.RootfsKey != "rootfs" || got.MemKey != "" || got.VMKey != "vm" {
		t.Fatalf("record = %#v", got)
	}
	if mode := BootMode(got); mode != state.TemplateBootModeCold {
		t.Fatalf("ready template boot mode = %q, want cold", mode)
	}

	items, err := store.List(ctx, Filter{Origin: state.TemplateOriginImage, BootMode: state.TemplateBootModeCold, Namespace: "team-a", State: state.TemplateReady})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != rec.ID {
		t.Fatalf("List() = %#v", items)
	}
}

func TestStoreCheckpointOriginRequiresMem(t *testing.T) {
	ctx := context.Background()
	store := NewStore(newTestStateStore(t))
	rec, err := store.Create(ctx, CreateRequest{Origin: state.TemplateOriginCheckpoint})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	err = store.MarkReady(ctx, rec.ID, Refs{RootfsKey: "rootfs", VMKey: "vm"})
	if err == nil || !strings.Contains(err.Error(), "mem key") {
		t.Fatalf("MarkReady() error = %v, want mem key error", err)
	}
}

func TestStoreMarkFailed(t *testing.T) {
	ctx := context.Background()
	store := NewStore(newTestStateStore(t))
	rec, err := store.Create(ctx, CreateRequest{Origin: state.TemplateOriginCheckpoint})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := store.MarkFailed(ctx, rec.ID, errBoom{}); err != nil {
		t.Fatalf("MarkFailed() error = %v", err)
	}
	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != state.TemplateFailed || got.LastError != "boom" {
		t.Fatalf("record = %#v", got)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

func newTestStateStore(t *testing.T) *state.BoltStore {
	t.Helper()
	store, err := state.OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})
	return store
}
