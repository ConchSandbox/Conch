package state

import (
	"context"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestBoltStoreSandboxCRUD(t *testing.T) {
	store := newBoltStore(t)
	ctx := context.Background()
	sandbox := SandboxRecord{
		SandboxID:                "sandbox-1",
		CheckpointHeadTemplateID: "template-1",
	}
	if err := store.UpsertSandbox(ctx, sandbox); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}
	sandbox.CheckpointHeadTemplateID = "template-replacement"
	if err := store.UpsertSandbox(ctx, sandbox); err != nil {
		t.Fatalf("UpsertSandbox(duplicate) error = %v", err)
	}
	got, err := store.GetSandbox(ctx, sandbox.SandboxID)
	if err != nil || got != sandbox {
		t.Fatalf("GetSandbox() = %#v, %v; want %#v", got, err, sandbox)
	}
	if err := store.DeleteSandbox(ctx, sandbox.SandboxID); err != nil {
		t.Fatalf("DeleteSandbox() error = %v", err)
	}
	if _, err := store.GetSandbox(ctx, sandbox.SandboxID); err == nil {
		t.Fatal("GetSandbox() after delete error = nil")
	}
}

func TestBoltStoreRejectsIncompleteSandboxRecord(t *testing.T) {
	store := newBoltStore(t)
	valid := SandboxRecord{
		SandboxID:                "sandbox-1",
		CheckpointHeadTemplateID: "template-1",
	}
	for _, tc := range []struct {
		name   string
		mutate func(*SandboxRecord)
	}{
		{name: "sandbox id", mutate: func(rec *SandboxRecord) { rec.SandboxID = "" }},
		{name: "checkpoint head", mutate: func(rec *SandboxRecord) { rec.CheckpointHeadTemplateID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := valid
			tc.mutate(&record)
			if err := store.UpsertSandbox(context.Background(), record); err == nil {
				t.Fatal("UpsertSandbox() error = nil")
			}
		})
	}
}

func TestBoltStoreAcceptsCreatingSandboxRecord(t *testing.T) {
	store := newBoltStore(t)
	record := SandboxRecord{
		SandboxID:        "sandbox-creating",
		State:            SandboxCreating,
		SourceTemplateID: "template-1",
	}
	if err := store.UpsertSandbox(context.Background(), record); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}
	got, err := store.GetSandbox(context.Background(), record.SandboxID)
	if err != nil || got != record {
		t.Fatalf("GetSandbox() = %#v, %v; want %#v", got, err, record)
	}
}

func TestBoltStoreInitializesOnlySandboxBucket(t *testing.T) {
	store := newBoltStore(t)
	if err := store.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("sandboxes")) == nil {
			t.Fatal("sandboxes bucket is missing")
		}
		if tx.Bucket([]byte("templates")) != nil {
			t.Fatal("templates bucket must not be owned by state.db")
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect state schema: %v", err)
	}
}

func TestBoltStoreAdvanceCheckpointHead(t *testing.T) {
	store := newBoltStore(t)
	ctx := context.Background()
	if err := store.UpsertSandbox(ctx, SandboxRecord{
		SandboxID:                "sandbox-1",
		CheckpointHeadTemplateID: "source",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceCheckpointHead(ctx, "sandbox-1", "source", "checkpoint"); err != nil {
		t.Fatalf("AdvanceCheckpointHead() error = %v", err)
	}
	record, err := store.GetSandbox(ctx, "sandbox-1")
	if err != nil || record.CheckpointHeadTemplateID != "checkpoint" {
		t.Fatalf("checkpoint head = %q, %v", record.CheckpointHeadTemplateID, err)
	}
}

func TestBoltStoreAdvanceCheckpointHeadCASFailureLeavesRecordUnchanged(t *testing.T) {
	store := newBoltStore(t)
	ctx := context.Background()
	if err := store.UpsertSandbox(ctx, SandboxRecord{
		SandboxID:                "sandbox-1",
		CheckpointHeadTemplateID: "current",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceCheckpointHead(ctx, "sandbox-1", "stale", "checkpoint"); err == nil {
		t.Fatal("AdvanceCheckpointHead() error = nil, want CAS failure")
	}
	record, err := store.GetSandbox(ctx, "sandbox-1")
	if err != nil || record.CheckpointHeadTemplateID != "current" {
		t.Fatalf("checkpoint head after CAS failure = %q, %v", record.CheckpointHeadTemplateID, err)
	}
}

func newBoltStore(t *testing.T) *BoltStore {
	t.Helper()
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store
}
