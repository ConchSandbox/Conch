package volumestore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/openeuler/Conch/internal/volume"
)

func TestStoreVolumeRecordRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "volumes.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	want := volume.VolumeRecord{ID: "vol-1", Name: "data", Token: "token-1", CreatedAt: 42}
	if err := store.Create(ctx, want); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	got, err := store.GetByName(ctx, "data")
	if err != nil {
		t.Fatalf("GetByName() error = %v", err)
	}
	if got != want {
		t.Fatalf("record = %#v, want %#v", got, want)
	}
	byID, err := store.Get(ctx, "vol-1")
	if err != nil || byID != want {
		t.Fatalf("Get(by id) = %#v, %v", byID, err)
	}
	listed, err := store.List(ctx)
	if err != nil || len(listed) != 1 || listed[0] != want {
		t.Fatalf("List() = %#v, %v", listed, err)
	}

	duplicate := want
	duplicate.ID = "vol-2"
	if err := store.Create(ctx, duplicate); !errors.Is(err, volume.ErrAlreadyExists) {
		t.Fatalf("duplicate name Create() = %v, want ErrAlreadyExists", err)
	}

	if err := store.Delete(ctx, "vol-1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.GetByName(ctx, "data"); !errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("GetByName() after delete = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(ctx, "vol-1"); !errors.Is(err, volume.ErrNotFound) {
		t.Fatalf("Get() after delete = %v, want ErrNotFound", err)
	}
}
