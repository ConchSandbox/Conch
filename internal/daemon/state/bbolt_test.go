package state

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"

	conchtemplate "github.com/openeuler/Conch/internal/template"
)

func TestNetworkSlotJSONUsesSingleNumericIdentity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		value      any
		idField    string
		legacyKeys []string
	}{
		{
			name:       "slot record",
			value:      NetworkSlotRecord{SlotID: 2},
			idField:    "slot_id",
			legacyKeys: []string{"slot_key", "slot_index", "netns_path", "cni_id"},
		},
		{
			name:       "sandbox record",
			value:      SandboxRecord{NetworkSlotID: 2},
			idField:    "network_slot_id",
			legacyKeys: []string{"network_slot_key", "network_ns"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if got := string(fields[tc.idField]); got != "2" {
				t.Fatalf("%s = %s, want numeric 2", tc.idField, got)
			}
			for _, legacyKey := range tc.legacyKeys {
				if _, ok := fields[legacyKey]; ok {
					t.Fatalf("legacy identity field %q is still serialized", legacyKey)
				}
			}
		})
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
		SlotID:    2,
		State:     NetworkSlotIdle,
		SandboxID: "sandbox-a",
		CNIIP:     "10.12.0.2",
		LastError: "initial error",
	}
	if err := store.CreateNetworkSlot(ctx, slot); err != nil {
		t.Fatalf("CreateNetworkSlot() error = %v", err)
	}
	got, err := store.GetNetworkSlot(ctx, slot.SlotID)
	if err != nil {
		t.Fatalf("GetNetworkSlot() error = %v", err)
	}
	if got.State != NetworkSlotIdle || got.CNIIP != slot.CNIIP || got.SandboxID != slot.SandboxID || got.LastError != slot.LastError {
		t.Fatalf("GetNetworkSlot() = %#v, want %#v", got, slot)
	}
	slot.State = NetworkSlotTransient
	slot.LastError = "cleanup pending"
	if err := store.UpdateNetworkSlot(ctx, slot); err != nil {
		t.Fatalf("UpdateNetworkSlot(update) error = %v", err)
	}
	got, err = store.GetNetworkSlot(ctx, slot.SlotID)
	if err != nil {
		t.Fatalf("GetNetworkSlot() after update error = %v", err)
	}
	if got.State != NetworkSlotTransient || got.LastError != "cleanup pending" || got.SandboxID != slot.SandboxID {
		t.Fatalf("GetNetworkSlot() after update = %#v, want transient slot", got)
	}
	slots, err := store.ListNetworkSlots(ctx)
	if err != nil {
		t.Fatalf("ListNetworkSlots() error = %v", err)
	}
	if len(slots) != 1 || slots[0].SlotID != slot.SlotID {
		t.Fatalf("ListNetworkSlots() = %#v, want one slot", slots)
	}
	if err := store.DeleteNetworkSlot(ctx, slot.SlotID); err != nil {
		t.Fatalf("DeleteNetworkSlot() error = %v", err)
	}
	if _, err := store.GetNetworkSlot(ctx, slot.SlotID); err == nil {
		t.Fatalf("GetNetworkSlot() after delete got nil error")
	}
}

func TestBoltStoreCreatesNetworkSlotInsertOnly(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	rec := NetworkSlotRecord{SlotID: 2, State: NetworkSlotTransient, UpdatedAt: 1}
	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.CreateNetworkSlot(ctx, rec)
		}()
	}
	wg.Wait()
	close(errs)
	succeeded := 0
	alreadyExists := 0
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrAlreadyExists):
			alreadyExists++
		default:
			t.Fatalf("CreateNetworkSlot() error = %v", err)
		}
	}
	if succeeded != 1 || alreadyExists != workers-1 {
		t.Fatalf("concurrent creates = (success=%d, already_exists=%d), want (1, %d)", succeeded, alreadyExists, workers-1)
	}
	if err := store.CreateNetworkSlot(ctx, NetworkSlotRecord{SlotID: 2, State: NetworkSlotIdle}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("CreateNetworkSlot() overwrite error = %v, want ErrAlreadyExists", err)
	}
	stored, err := store.GetNetworkSlot(ctx, 2)
	if err != nil {
		t.Fatalf("GetNetworkSlot() error = %v", err)
	}
	if stored.State != NetworkSlotTransient || stored.UpdatedAt != 1 {
		t.Fatalf("stored slot = %#v, want original transient record", stored)
	}
}

func TestBoltStoreUpdateNetworkSlotRequiresAllocation(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	err = store.UpdateNetworkSlot(context.Background(), NetworkSlotRecord{SlotID: 2, State: NetworkSlotIdle})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateNetworkSlot() error = %v, want ErrNotFound", err)
	}
}

func TestBoltStoreUpdateNetworkSlotCannotMoveRecordToAnotherID(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	rec := NetworkSlotRecord{SlotID: 2, State: NetworkSlotTransient}
	if err := store.CreateNetworkSlot(ctx, rec); err != nil {
		t.Fatalf("CreateNetworkSlot() error = %v", err)
	}
	rec.SlotID = 3
	if err := store.UpdateNetworkSlot(ctx, rec); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateNetworkSlot() error = %v, want ErrNotFound", err)
	}

	stored, err := store.GetNetworkSlot(ctx, 2)
	if err != nil {
		t.Fatalf("GetNetworkSlot() error = %v", err)
	}
	if stored.SlotID != 2 || stored.State != NetworkSlotTransient {
		t.Fatalf("stored slot identity changed: %#v", stored)
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
	if _, err := store.GetTemplate(ctx, "t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTemplate() error = %v, want ErrNotFound", err)
	}
	sandboxRecord, _ := store.GetSandbox(ctx, "sandbox-1")
	if sandboxRecord.CheckpointHeadTemplateID != "new-head" {
		t.Fatalf("sandbox changed after failed transaction: %#v", sandboxRecord)
	}
}
