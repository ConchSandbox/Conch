package state

import (
	"context"
	"testing"
)

func TestBoltStoreSandboxAndContainerCRUD(t *testing.T) {
	store, err := OpenBolt(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("OpenBolt() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sandbox := SandboxRecord{
		PodSandboxID:   "pod-1",
		ConchSandboxID: "pod-1",
		Namespace:      "default",
		Name:           "demo",
		State:          SandboxReady,
		Labels:         map[string]string{"app": "demo"},
	}
	if err := store.UpsertSandbox(ctx, sandbox); err != nil {
		t.Fatalf("UpsertSandbox() error = %v", err)
	}
	gotSandbox, err := store.GetSandbox(ctx, sandbox.PodSandboxID)
	if err != nil {
		t.Fatalf("GetSandbox() error = %v", err)
	}
	if gotSandbox.Name != sandbox.Name || gotSandbox.Labels["app"] != "demo" {
		t.Fatalf("GetSandbox() = %#v, want %#v", gotSandbox, sandbox)
	}

	container := ContainerRecord{
		ContainerID:  "ctr-1",
		PodSandboxID: sandbox.PodSandboxID,
		Name:         "placeholder",
		State:        ContainerCreated,
	}
	if err := store.UpsertContainer(ctx, container); err != nil {
		t.Fatalf("UpsertContainer() error = %v", err)
	}
	containers, err := store.ListContainers(ctx)
	if err != nil {
		t.Fatalf("ListContainers() error = %v", err)
	}
	if len(containers) != 1 || containers[0].ContainerID != container.ContainerID {
		t.Fatalf("ListContainers() = %#v, want one container", containers)
	}

	if err := store.DeleteSandbox(ctx, sandbox.PodSandboxID); err != nil {
		t.Fatalf("DeleteSandbox() error = %v", err)
	}
	if _, err := store.GetSandbox(ctx, sandbox.PodSandboxID); err == nil {
		t.Fatalf("GetSandbox() after delete got nil error")
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
	rec := TemplateRecord{
		ID:        "tmpl_1",
		Origin:    TemplateOriginImage,
		Namespace: "default",
		State:     TemplateCreating,
		Labels:    map[string]string{"purpose": "test"},
		CreatedAt: 1,
		UpdatedAt: 1,
	}
	if err := store.UpsertTemplate(ctx, rec); err != nil {
		t.Fatalf("UpsertTemplate() error = %v", err)
	}
	got, err := store.GetTemplate(ctx, rec.ID)
	if err != nil {
		t.Fatalf("GetTemplate() error = %v", err)
	}
	if got.Origin != rec.Origin || got.Labels["purpose"] != "test" {
		t.Fatalf("GetTemplate() = %#v, want %#v", got, rec)
	}

	rec.State = TemplateReady
	rec.RootfsKey = "rootfs"
	rec.VMKey = "vm"
	if err := store.UpsertTemplate(ctx, rec); err != nil {
		t.Fatalf("UpsertTemplate(update) error = %v", err)
	}
	items, err := store.ListTemplates(ctx)
	if err != nil {
		t.Fatalf("ListTemplates() error = %v", err)
	}
	if len(items) != 1 || items[0].State != TemplateReady {
		t.Fatalf("ListTemplates() = %#v", items)
	}
	if err := store.DeleteTemplate(ctx, rec.ID); err != nil {
		t.Fatalf("DeleteTemplate() error = %v", err)
	}
	if _, err := store.GetTemplate(ctx, rec.ID); err == nil {
		t.Fatalf("GetTemplate() after delete got nil error")
	}
}
