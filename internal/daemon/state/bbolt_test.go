package state

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"

	conchtemplate "github.com/openeuler/Conch/internal/template"
)

func TestBoltStoreSandboxCRUD(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sandbox := SandboxRecord{
		SandboxID:                "sandbox-1",
		CheckpointHeadTemplateID: "template-1",
	}
	if err := store.UpsertSandbox(ctx, sandbox); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}
	duplicate := sandbox
	duplicate.CheckpointHeadTemplateID = "template-replacement"
	if err := store.UpsertSandbox(ctx, duplicate); err != nil {
		t.Fatalf("UpsertSandbox(duplicate) error = %v", err)
	}
	gotSandbox, err := store.GetSandbox(ctx, sandbox.SandboxID)
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if gotSandbox != duplicate {
		t.Fatalf("GetSandbox() = %#v, want %#v", gotSandbox, duplicate)
	}

	if err := store.DeleteSandbox(ctx, sandbox.SandboxID); err != nil {
		t.Fatalf("DeleteSandbox() error = %v", err)
	}
	if _, err := store.GetSandbox(ctx, sandbox.SandboxID); err == nil {
		t.Fatalf("GetSandbox() after delete got nil error")
	}
}

func TestBoltStoreRejectsIncompleteSandboxRecord(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	valid := SandboxRecord{
		SandboxID:                "sandbox-1",
		CheckpointHeadTemplateID: "template-1",
	}
	tests := []struct {
		name   string
		mutate func(*SandboxRecord)
	}{
		{name: "sandbox id", mutate: func(rec *SandboxRecord) { rec.SandboxID = "" }},
		{name: "checkpoint head digest", mutate: func(rec *SandboxRecord) { rec.CheckpointHeadTemplateID = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := valid
			tc.mutate(&rec)
			if err := store.UpsertSandbox(context.Background(), rec); err == nil {
				t.Fatal("UpsertSandbox() error = nil, want incomplete record rejection")
			}
		})
	}
}

func TestBoltStoreAcceptsCreatingSandboxRecord(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	rec := SandboxRecord{
		SandboxID:        "sandbox-creating",
		State:            SandboxCreating,
		SourceTemplateID: "template-1",
	}
	if err := store.UpsertSandbox(context.Background(), rec); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}
	got, err := store.GetSandbox(context.Background(), rec.SandboxID)
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if got != rec {
		t.Fatalf("GetSandbox() = %#v, want %#v", got, rec)
	}
}

func TestBoltStoreInitializesCurrentBuckets(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	if err := store.db.View(func(tx *bolt.Tx) error {
		for _, bucket := range buckets {
			if tx.Bucket(bucket) == nil {
				t.Fatalf("bucket %q is missing", bucket)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect state schema: %v", err)
	}
}

func TestBoltStoreTemplateCRUD(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	rec := conchtemplate.Entry{
		Origin:          conchtemplate.OriginImage,
		BootMode:        conchtemplate.BootModeCold,
		BootIndexDigest: digest.FromString("template-1").String(),
		Labels:          map[string]string{"purpose": "test"},
		CreatedAt:       1,
	}
	if err := store.CreateTemplate(ctx, rec); err != nil {
		t.Fatalf("CreateTemplate() error = %v", err)
	}
	got, err := store.GetTemplate(ctx, rec.BootIndexDigest)
	if err != nil {
		t.Fatalf("GetTemplate() error = %v", err)
	}
	if got.Origin != rec.Origin || got.Labels["purpose"] != "test" {
		t.Fatalf("GetTemplate() = %#v, want %#v", got, rec)
	}

	duplicate := rec
	duplicate.Origin = conchtemplate.OriginCheckpoint
	duplicate.BootMode = conchtemplate.BootModeResume
	if err := store.CreateTemplate(ctx, duplicate); !errors.Is(err, conchtemplate.ErrAlreadyExists) {
		t.Fatalf("CreateTemplate(duplicate) error = %v, want ErrAlreadyExists", err)
	}
	got, err = store.GetTemplate(ctx, rec.BootIndexDigest)
	if err != nil {
		t.Fatalf("GetTemplate() after duplicate error = %v", err)
	}
	if got.BootIndexDigest != rec.BootIndexDigest {
		t.Fatalf("duplicate CreateTemplate overwrote digest: got %q, want %q", got.BootIndexDigest, rec.BootIndexDigest)
	}
	items, err := store.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("ListTemplates() error = %v", err)
	}
	if len(items) != 1 || items[0].BootIndexDigest != rec.BootIndexDigest {
		t.Fatalf("ListTemplates() = %#v", items)
	}
	if err := store.DeleteTemplate(ctx, rec.BootIndexDigest); err != nil {
		t.Fatalf("DeleteTemplate() error = %v", err)
	}
	if err := store.DeleteTemplate(ctx, rec.BootIndexDigest); err != nil {
		t.Fatalf("DeleteTemplate() second call error = %v", err)
	}
	if _, err := store.GetTemplate(ctx, rec.BootIndexDigest); err == nil {
		t.Fatalf("GetTemplate() after delete got nil error")
	}
}

func TestBoltStoreLeavesTemplateValidationToDomain(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	record := templateRecord{
		Origin:          string(conchtemplate.OriginImage),
		BootMode:        string(conchtemplate.BootModeCold),
		BootIndexDigest: "not-a-digest",
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("templates")).Put([]byte(record.BootIndexDigest), data)
	}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	got, err := store.GetTemplate(ctx, record.BootIndexDigest)
	if err != nil {
		t.Fatalf("GetTemplate() error = %v", err)
	}
	if got.BootIndexDigest != record.BootIndexDigest {
		t.Fatalf("GetTemplate() = %#v", got)
	}
	items, err := store.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("ListTemplates() error = %v", err)
	}
	if len(items) != 1 || items[0].BootIndexDigest != record.BootIndexDigest {
		t.Fatalf("ListTemplates() = %#v", items)
	}
	if _, err := conchtemplate.NewStore(store).List(ctx, conchtemplate.Filter{}); err == nil {
		t.Fatal("domain List() error = nil, want invalid digest rejection")
	}
}

func TestBoltStorePublishCheckpointAdvancesHeadAtomically(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	checkpointDigest := digest.FromString("checkpoint").String()
	sourceDigest := digest.FromString("source").String()
	if err := store.UpsertSandbox(ctx, SandboxRecord{
		SandboxID:                "sb-1",
		CheckpointHeadTemplateID: sourceDigest,
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.PublishCheckpoint(ctx, conchtemplate.Entry{
		Origin:                conchtemplate.OriginCheckpoint,
		BootMode:              conchtemplate.BootModeResume,
		BootIndexDigest:       checkpointDigest,
		ParentBootIndexDigest: sourceDigest,
		SourceSandboxID:       "sb-1",
	}); err != nil {
		t.Fatalf("PublishCheckpoint() error = %v", err)
	}
	templateRecord, err := store.GetTemplate(ctx, checkpointDigest)
	if err != nil {
		t.Fatal(err)
	}
	if templateRecord.BootIndexDigest != checkpointDigest ||
		templateRecord.Origin != conchtemplate.OriginCheckpoint ||
		templateRecord.BootMode != conchtemplate.BootModeResume {
		t.Fatalf("published template = %#v", templateRecord)
	}
	sandboxRecord, err := store.GetSandbox(ctx, "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if sandboxRecord.CheckpointHeadTemplateID != checkpointDigest {
		t.Fatalf("checkpoint head = %#v", sandboxRecord)
	}
}

func TestBoltStorePublishCheckpointCASFailureLeavesBothRecordsUnchanged(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	newHeadDigest := digest.FromString("new-head").String()
	oldHeadDigest := digest.FromString("old-head").String()
	checkpointDigest := digest.FromString("checkpoint").String()
	if err := store.UpsertSandbox(ctx, SandboxRecord{
		SandboxID:                "sandbox-1",
		CheckpointHeadTemplateID: newHeadDigest,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishCheckpoint(ctx, conchtemplate.Entry{
		Origin:                conchtemplate.OriginCheckpoint,
		BootMode:              conchtemplate.BootModeResume,
		BootIndexDigest:       checkpointDigest,
		ParentBootIndexDigest: oldHeadDigest,
		SourceSandboxID:       "sandbox-1",
	}); err == nil {
		t.Fatal("PublishCheckpoint() error = nil, want CAS failure")
	}
	if _, err := store.GetTemplate(ctx, checkpointDigest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTemplate() error = %v, want ErrNotFound", err)
	}
	sandboxRecord, _ := store.GetSandbox(ctx, "sandbox-1")
	if sandboxRecord.CheckpointHeadTemplateID != newHeadDigest {
		t.Fatalf("sandbox changed after failed transaction: %#v", sandboxRecord)
	}
}
