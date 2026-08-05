package state

import (
	"context"
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
		SandboxID: "sandbox-1",
		Namespace: "default",
		State:     SandboxReady,
		CreatedAt: 123,
		IP:        "192.0.2.10",
		VMMName:   "stratovirt",
	}
	if err := store.UpsertSandbox(ctx, sandbox); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}
	gotSandbox, err := store.GetSandbox(ctx, sandbox.SandboxID)
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if gotSandbox.SandboxID != sandbox.SandboxID || gotSandbox.IP != sandbox.IP || gotSandbox.VMMName != sandbox.VMMName {
		t.Fatalf("GetSandbox() = %#v, want %#v", gotSandbox, sandbox)
	}

	if err := store.DeleteSandbox(ctx, sandbox.SandboxID); err != nil {
		t.Fatalf("DeleteSandbox() error = %v", err)
	}
	if _, err := store.GetSandbox(ctx, sandbox.SandboxID); err == nil {
		t.Fatalf("GetSandbox() after delete got nil error")
	}
}

func TestBoltStoreRejectsEmptySandboxID(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	if err := store.UpsertSandbox(context.Background(), SandboxRecord{}); err == nil {
		t.Fatal("UpsertSandbox() error = nil, want empty id rejection")
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
		if tx.Bucket([]byte("containers")) != nil {
			t.Fatal("containers bucket was created")
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect state schema: %v", err)
	}
}

func TestBoltStoreNetworkSlotCRUD(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	slot := NetworkSlotRecord{
		SlotKey:   "2",
		SlotIndex: 2,
		State:     NetworkSlotWarmIdle,
		SandboxID: "sandbox-a",
		NetNSPath: "/var/run/netns/ns-2",
		CNIID:     "conch-slot-2",
		CNIIP:     "10.12.0.2",
		LastError: "initial error",
	}
	if err := store.UpsertNetworkSlot(ctx, slot); err != nil {
		t.Fatalf("UpsertNetworkSlot() error = %v", err)
	}
	got, err := store.GetNetworkSlot(ctx, slot.SlotKey)
	if err != nil {
		t.Fatalf("GetNetworkSlot() error = %v", err)
	}
	if got.State != NetworkSlotWarmIdle || got.CNIID != slot.CNIID || got.SandboxID != slot.SandboxID || got.LastError != slot.LastError {
		t.Fatalf("GetNetworkSlot() = %#v, want %#v", got, slot)
	}
	slot.State = NetworkSlotCleaning
	slot.LastError = "cleanup pending"
	if err := store.UpsertNetworkSlot(ctx, slot); err != nil {
		t.Fatalf("UpsertNetworkSlot(update) error = %v", err)
	}
	got, err = store.GetNetworkSlot(ctx, slot.SlotKey)
	if err != nil {
		t.Fatalf("GetNetworkSlot() after update error = %v", err)
	}
	if got.State != NetworkSlotCleaning || got.LastError != "cleanup pending" || got.SandboxID != slot.SandboxID {
		t.Fatalf("GetNetworkSlot() after update = %#v, want cleaning slot", got)
	}
	slots, err := store.ListNetworkSlots(ctx)
	if err != nil {
		t.Fatalf("ListNetworkSlots() error = %v", err)
	}
	if len(slots) != 1 || slots[0].SlotKey != slot.SlotKey {
		t.Fatalf("ListNetworkSlots() = %#v, want one slot", slots)
	}
	if err := store.DeleteNetworkSlot(ctx, slot.SlotKey); err != nil {
		t.Fatalf("DeleteNetworkSlot() error = %v", err)
	}
	if _, err := store.GetNetworkSlot(ctx, slot.SlotKey); err == nil {
		t.Fatalf("GetNetworkSlot() after delete got nil error")
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
		SandboxID:        "sb-1",
		Namespace:        "default",
		SourceTemplateID: "t0", SourceBootIndexDigest: "sha256:source",
		CheckpointHeadTemplateID: "t0", CheckpointHeadBootIndexDigest: "sha256:source",
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.PublishCheckpoint(ctx, CheckpointPublication{
		Entry: conchtemplate.Entry{
			ID:               "t1",
			Origin:           conchtemplate.OriginCheckpoint,
			BootMode:         conchtemplate.BootModeResume,
			BootIndexDigest:  checkpointDigest,
			Namespace:        "default",
			ParentTemplateID: "t0",
			SourceSandboxID:  "sb-1",
		},
		SandboxID: "sb-1", ExpectedHeadTemplateID: "t0",
		ExpectedHeadBootIndexDigest: "sha256:source",
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
	if err := store.PublishCheckpoint(ctx, CheckpointPublication{
		Entry: conchtemplate.Entry{
			ID:               "t1",
			Origin:           conchtemplate.OriginCheckpoint,
			BootMode:         conchtemplate.BootModeResume,
			BootIndexDigest:  digest.FromString("checkpoint").String(),
			Namespace:        "default",
			ParentTemplateID: "old-head",
			SourceSandboxID:  "sandbox-1",
		},
		SandboxID:                   "sandbox-1",
		ExpectedHeadTemplateID:      "old-head",
		ExpectedHeadBootIndexDigest: "sha256:old-head",
	}); err == nil {
		t.Fatal("PublishCheckpoint() error = nil, want CAS failure")
	}
	_, err = store.GetTemplate(ctx, "t1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTemplate() error = %v, want ErrNotFound", err)
	}
	if !errors.Is(err, conchtemplate.ErrNotFound) {
		t.Fatalf("GetTemplate() error = %v, want template.ErrNotFound", err)
	}
	sandboxRecord, _ := store.GetSandbox(ctx, "sandbox-1")
	if sandboxRecord.CheckpointHeadTemplateID != "new-head" {
		t.Fatalf("sandbox changed after failed transaction: %#v", sandboxRecord)
	}
}
