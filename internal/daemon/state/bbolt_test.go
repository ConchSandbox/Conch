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

func TestSandboxRecordJSONContainsOnlyCheckpointMetadata(t *testing.T) {
	rec := SandboxRecord{
		SandboxID:                     "sandbox-1",
		Namespace:                     "default",
		CheckpointHeadTemplateID:      "tmpl-1",
		CheckpointHeadBootIndexDigest: digest.FromString("boot-index").String(),
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	want := []string{
		"sandbox_id",
		"namespace",
		"checkpoint_head_template_id",
		"checkpoint_head_boot_index_digest",
	}
	if len(fields) != len(want) {
		t.Fatalf("sandbox record fields = %#v, want %v", fields, want)
	}
	for _, key := range want {
		if _, ok := fields[key]; !ok {
			t.Fatalf("sandbox record is missing field %q", key)
		}
	}
}

func TestBoltStoreSandboxCRUD(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sandbox := SandboxRecord{
		SandboxID:                     "sandbox-1",
		Namespace:                     "default",
		CheckpointHeadTemplateID:      "tmpl-1",
		CheckpointHeadBootIndexDigest: digest.FromString("boot-index").String(),
	}
	if err := store.UpsertSandbox(ctx, sandbox); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}
	gotSandbox, err := store.GetSandbox(ctx, sandbox.SandboxID)
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if gotSandbox != sandbox {
		t.Fatalf("GetSandbox() = %#v, want %#v", gotSandbox, sandbox)
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
		SandboxID:                     "sandbox-1",
		Namespace:                     "default",
		CheckpointHeadTemplateID:      "tmpl-1",
		CheckpointHeadBootIndexDigest: digest.FromString("boot-index").String(),
	}
	tests := []struct {
		name   string
		mutate func(*SandboxRecord)
	}{
		{name: "sandbox id", mutate: func(rec *SandboxRecord) { rec.SandboxID = "" }},
		{name: "namespace", mutate: func(rec *SandboxRecord) { rec.Namespace = "" }},
		{name: "checkpoint head template", mutate: func(rec *SandboxRecord) { rec.CheckpointHeadTemplateID = "" }},
		{name: "checkpoint head digest", mutate: func(rec *SandboxRecord) { rec.CheckpointHeadBootIndexDigest = "" }},
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
		ID:              "tmpl_1",
		Origin:          conchtemplate.OriginImage,
		BootMode:        conchtemplate.BootModeCold,
		BootIndexDigest: digest.FromString("template-1").String(),
		Namespace:       "default",
		Labels:          map[string]string{"purpose": "test"},
		CreatedAt:       1,
	}
	if err := store.CreateTemplate(ctx, rec); err != nil {
		t.Fatalf("CreateTemplate() error = %v", err)
	}
	got, err := store.GetTemplate(ctx, rec.ID)
	if err != nil {
		t.Fatalf("GetTemplate() error = %v", err)
	}
	if got.Origin != rec.Origin || got.Labels["purpose"] != "test" {
		t.Fatalf("GetTemplate() = %#v, want %#v", got, rec)
	}

	duplicate := rec
	duplicate.BootIndexDigest = digest.FromString("replacement").String()
	if err := store.CreateTemplate(ctx, duplicate); !errors.Is(err, conchtemplate.ErrAlreadyExists) {
		t.Fatalf("CreateTemplate(duplicate) error = %v, want ErrAlreadyExists", err)
	}
	got, err = store.GetTemplate(ctx, rec.ID)
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
	if len(items) != 1 || items[0].ID != rec.ID {
		t.Fatalf("ListTemplates() = %#v", items)
	}
	if err := store.DeleteTemplate(ctx, rec.ID); err != nil {
		t.Fatalf("DeleteTemplate() error = %v", err)
	}
	if _, err := store.GetTemplate(ctx, rec.ID); err == nil {
		t.Fatalf("GetTemplate() after delete got nil error")
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
	if err := store.UpsertSandbox(ctx, SandboxRecord{
		SandboxID:                     "sb-1",
		Namespace:                     "default",
		CheckpointHeadTemplateID:      "t0",
		CheckpointHeadBootIndexDigest: "sha256:source",
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.PublishCheckpoint(ctx, conchtemplate.Entry{
		ID:               "t1",
		Origin:           conchtemplate.OriginCheckpoint,
		BootMode:         conchtemplate.BootModeResume,
		BootIndexDigest:  checkpointDigest,
		Namespace:        "default",
		ParentTemplateID: "t0",
		SourceSandboxID:  "sb-1",
	}); err != nil {
		t.Fatalf("PublishCheckpoint() error = %v", err)
	}
	templateRecord, err := store.GetTemplate(ctx, "t1")
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
	if sandboxRecord.CheckpointHeadTemplateID != "t1" || sandboxRecord.CheckpointHeadBootIndexDigest != checkpointDigest {
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
	if err := store.UpsertSandbox(ctx, SandboxRecord{
		SandboxID:                     "sandbox-1",
		Namespace:                     "default",
		CheckpointHeadTemplateID:      "new-head",
		CheckpointHeadBootIndexDigest: "sha256:new-head",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishCheckpoint(ctx, conchtemplate.Entry{
		ID:               "t1",
		Origin:           conchtemplate.OriginCheckpoint,
		BootMode:         conchtemplate.BootModeResume,
		BootIndexDigest:  digest.FromString("checkpoint").String(),
		Namespace:        "default",
		ParentTemplateID: "old-head",
		SourceSandboxID:  "sandbox-1",
	}); err == nil {
		t.Fatal("PublishCheckpoint() error = nil, want CAS failure")
	}
	if _, err := store.GetTemplate(ctx, "t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTemplate() error = %v, want ErrNotFound", err)
	}
	sandboxRecord, _ := store.GetSandbox(ctx, "sandbox-1")
	if sandboxRecord.CheckpointHeadTemplateID != "new-head" {
		t.Fatalf("sandbox changed after failed transaction: %#v", sandboxRecord)
	}
}
