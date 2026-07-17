package template

import (
	"context"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/openeuler/Conch/internal/daemon/state"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/snapshot"
)

func TestManagerColdCreateResolvesBootIndexWithoutSnapshotInfo(t *testing.T) {
	ctx := context.Background()
	store, rec, bootDigest := newReadyResolverTemplate(t, state.TemplateOriginImage, "team-a", state.TemplateBootModeCold)
	bootIndexes := &fakeBootIndexBackend{result: resolvedBootIndex(bootDigest, false, "")}
	snapshots := &fakeSnapshotBackend{}
	manager := NewManager(store, snapshots, bootIndexes)

	got, err := manager.PrepareSandboxBoot(ctx, PrepareSandboxBootRequest{
		Namespace:  "team-a",
		TemplateID: rec.ID,
		SandboxID:  "sandbox-a",
		VMMName:    "cloud-hypervisor",
		RamMB:      512,
	})
	if err != nil {
		t.Fatalf("PrepareSandboxBoot() error = %v", err)
	}
	assertBootIndexRequest(t, bootIndexes.requests, "team-a", bootDigest)
	if len(snapshots.creates) != 1 || len(snapshots.restores) != 0 {
		t.Fatalf("snapshot calls: creates=%#v restores=%#v", snapshots.creates, snapshots.restores)
	}
	call := snapshots.creates[0]
	if call.namespace != "team-a" || call.key != "sandbox-a" || call.memorySizeMB != 512 {
		t.Fatalf("cold create call = %#v", call)
	}
	if call.memoryLayout != snapshot.MemoryLayoutWritableFile {
		t.Fatalf("cold memory layout = %q", call.memoryLayout)
	}
	if call.parents != (snapshot.ParentSnapshotIDs{Rootfs: "rootfs-committed", VM: "vm-committed"}) {
		t.Fatalf("cold parents = %#v", call.parents)
	}
	if got.Runtime.Resume || got.Runtime.BootIndexDigest != bootDigest || got.Runtime.CapturedVMMName != "" {
		t.Fatalf("cold runtime = %#v", got.Runtime)
	}
	if got.Runtime.RootfsKey != "sandbox-a" || got.Runtime.MemKey != "sandbox-a-mem" {
		t.Fatalf("runtime handles = %#v", got.Runtime)
	}
	if got.Spec.MemorySizeMB != 512 || !strings.Contains(got.Spec.MemoryPath, "sandbox-a") {
		t.Fatalf("cold boot spec = %#v", got.Spec)
	}
}

func TestManagerStratovirtColdCreateUsesNoMemoryLayer(t *testing.T) {
	ctx := context.Background()
	store, rec, bootDigest := newReadyResolverTemplate(t, state.TemplateOriginImage, "team-a", state.TemplateBootModeCold)
	bootIndexes := &fakeBootIndexBackend{result: resolvedBootIndex(bootDigest, false, "")}
	snapshots := &fakeSnapshotBackend{}

	got, err := NewManager(store, snapshots, bootIndexes).PrepareSandboxBoot(ctx, PrepareSandboxBootRequest{
		Namespace:  "team-a",
		TemplateID: rec.ID,
		SandboxID:  "sandbox-stratovirt",
		VMMName:    "stratovirt",
		RamMB:      768,
	})
	if err != nil {
		t.Fatalf("PrepareSandboxBoot() error = %v", err)
	}
	if len(snapshots.creates) != 1 || snapshots.creates[0].memoryLayout != snapshot.MemoryLayoutNone {
		t.Fatalf("create calls = %#v", snapshots.creates)
	}
	if snapshots.creates[0].memorySizeMB != 768 {
		t.Fatalf("memory size = %d", snapshots.creates[0].memorySizeMB)
	}
	if got.Spec.MemoryPath != "" || got.Spec.SnapfilePath != "" {
		t.Fatalf("StratoVirt cold spec = %#v", got.Spec)
	}
	if got.Runtime.MemKey != "" || got.Runtime.MemMount != "" {
		t.Fatalf("StratoVirt cold runtime = %#v", got.Runtime)
	}
}

func TestManagerResumeRestoresResolvedBootIndex(t *testing.T) {
	ctx := context.Background()
	store, rec, bootDigest := newReadyResolverTemplate(t, state.TemplateOriginCheckpoint, "team-a", state.TemplateBootModeResume)
	bootIndexes := &fakeBootIndexBackend{result: resolvedBootIndex(bootDigest, true, "cloud-hypervisor")}
	snapshots := &fakeSnapshotBackend{}
	manager := NewManager(store, snapshots, bootIndexes)

	got, err := manager.PrepareSandboxBoot(ctx, PrepareSandboxBootRequest{
		Namespace:  "team-a",
		TemplateID: rec.ID,
		SandboxID:  "sandbox-a",
		VMMName:    "cloud-hypervisor",
	})
	if err != nil {
		t.Fatalf("PrepareSandboxBoot() error = %v", err)
	}
	assertBootIndexRequest(t, bootIndexes.requests, "team-a", bootDigest)
	if len(snapshots.restores) != 1 || len(snapshots.creates) != 0 {
		t.Fatalf("snapshot calls: creates=%#v restores=%#v", snapshots.creates, snapshots.restores)
	}
	call := snapshots.restores[0]
	if call.namespace != "team-a" || call.key != "sandbox-a" {
		t.Fatalf("restore call = %#v", call)
	}
	if call.memoryLayout != snapshot.MemoryLayoutWritableFile || call.memorySizeMB != 256 {
		t.Fatalf("restore memory request = %#v", call)
	}
	if call.parents != (snapshot.ParentSnapshotIDs{Rootfs: "rootfs-committed", Mem: "mem-committed", VM: "vm-committed"}) {
		t.Fatalf("resume parents = %#v", call.parents)
	}
	if !got.Runtime.Resume || got.Runtime.BootIndexDigest != bootDigest || got.Runtime.CapturedVMMName != "cloud-hypervisor" {
		t.Fatalf("resume runtime = %#v", got.Runtime)
	}
	if got.Spec.SnapfilePath == "" {
		t.Fatalf("resume boot = %#v", got)
	}
}

func TestManagerRejectsTemplateNamespaceMismatchBeforeDigestResolution(t *testing.T) {
	store, rec, _ := newReadyResolverTemplate(t, state.TemplateOriginImage, "team-a", state.TemplateBootModeCold)
	bootIndexes := &fakeBootIndexBackend{}
	snapshots := &fakeSnapshotBackend{}

	_, err := NewManager(store, snapshots, bootIndexes).PrepareSandboxBoot(context.Background(), PrepareSandboxBootRequest{
		Namespace:  "team-b",
		TemplateID: rec.ID,
		SandboxID:  "sandbox-a",
	})
	if err == nil || !strings.Contains(err.Error(), "belongs to namespace team-a, not team-b") {
		t.Fatalf("PrepareSandboxBoot() error = %v, want namespace mismatch", err)
	}
	if len(bootIndexes.requests) != 0 || snapshots.callCount() != 0 {
		t.Fatalf("backends called after namespace mismatch: boot=%#v snapshots=%#v", bootIndexes.requests, snapshots)
	}
}

func TestManagerRejectsMissingBootIndexDigest(t *testing.T) {
	rec := state.TemplateRecord{
		ID:        "tmpl_missing",
		Origin:    state.TemplateOriginImage,
		Namespace: "team-a",
		State:     state.TemplateReady,
		BootMode:  state.TemplateBootModeCold,
	}
	raw := newResolverStateStore(t)
	if err := raw.UpsertTemplate(context.Background(), rec); err != nil {
		t.Fatalf("UpsertTemplate() error = %v", err)
	}
	bootIndexes := &fakeBootIndexBackend{}
	snapshots := &fakeSnapshotBackend{}
	_, err := NewManager(NewStore(raw), snapshots, bootIndexes).PrepareSandboxBoot(context.Background(), PrepareSandboxBootRequest{
		Namespace:  "team-a",
		TemplateID: rec.ID,
		SandboxID:  "sandbox-a",
	})
	if err == nil || !strings.Contains(err.Error(), "has no boot index digest") {
		t.Fatalf("PrepareSandboxBoot() error = %v, want missing digest error", err)
	}
	if len(bootIndexes.requests) != 0 || snapshots.callCount() != 0 {
		t.Fatalf("backends called for missing digest: boot=%#v snapshots=%#v", bootIndexes.requests, snapshots)
	}
}

func TestManagerRejectsCachedCapabilityMismatch(t *testing.T) {
	for _, tt := range []struct {
		name         string
		cachedMode   string
		resolvedMode string
	}{
		{name: "cached cold resolved resume", cachedMode: state.TemplateBootModeCold, resolvedMode: state.TemplateBootModeResume},
		{name: "cached resume resolved cold", cachedMode: state.TemplateBootModeResume, resolvedMode: state.TemplateBootModeCold},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, rec, bootDigest := newReadyResolverTemplate(t, state.TemplateOriginImage, "team-a", tt.cachedMode)
			resume := tt.resolvedMode == state.TemplateBootModeResume
			vmmName := ""
			if resume {
				vmmName = "cloud-hypervisor"
			}
			bootIndexes := &fakeBootIndexBackend{result: resolvedBootIndex(bootDigest, resume, vmmName)}
			snapshots := &fakeSnapshotBackend{}
			_, err := NewManager(store, snapshots, bootIndexes).PrepareSandboxBoot(context.Background(), PrepareSandboxBootRequest{
				Namespace:  "team-a",
				TemplateID: rec.ID,
				SandboxID:  "sandbox-a",
			})
			if err == nil || !strings.Contains(err.Error(), "cached boot mode") {
				t.Fatalf("PrepareSandboxBoot() error = %v, want capability mismatch", err)
			}
			if snapshots.callCount() != 0 {
				t.Fatalf("snapshot backend called for mismatched capability: %#v", snapshots)
			}
		})
	}
}

func TestManagerRejectsResumeVMMMismatch(t *testing.T) {
	store, rec, bootDigest := newReadyResolverTemplate(t, state.TemplateOriginCheckpoint, "team-a", state.TemplateBootModeResume)
	bootIndexes := &fakeBootIndexBackend{result: resolvedBootIndex(bootDigest, true, "cloud-hypervisor")}
	snapshots := &fakeSnapshotBackend{}

	_, err := NewManager(store, snapshots, bootIndexes).PrepareSandboxBoot(context.Background(), PrepareSandboxBootRequest{
		Namespace:  "team-a",
		TemplateID: rec.ID,
		SandboxID:  "sandbox-a",
		VMMName:    "stratovirt",
	})
	if err == nil || !strings.Contains(err.Error(), "captured by VMM cloud-hypervisor, not stratovirt") {
		t.Fatalf("PrepareSandboxBoot() error = %v, want VMM mismatch", err)
	}
	if snapshots.callCount() != 0 {
		t.Fatalf("snapshot backend called for VMM mismatch: %#v", snapshots)
	}
}

func TestManagerRejectsStratovirtResumeWithoutMemorySize(t *testing.T) {
	store, rec, bootDigest := newReadyResolverTemplate(t, state.TemplateOriginCheckpoint, "team-a", state.TemplateBootModeResume)
	resolved := resolvedBootIndex(bootDigest, true, "stratovirt")
	resolved.MemorySizeMB = 0
	snapshots := &fakeSnapshotBackend{}

	_, err := NewManager(store, snapshots, &fakeBootIndexBackend{result: resolved}).PrepareSandboxBoot(context.Background(), PrepareSandboxBootRequest{
		Namespace:  "team-a",
		TemplateID: rec.ID,
		SandboxID:  "sandbox-a",
		VMMName:    "stratovirt",
	})
	if err == nil || !strings.Contains(err.Error(), "missing memory size") {
		t.Fatalf("PrepareSandboxBoot() error = %v", err)
	}
	if snapshots.callCount() != 0 {
		t.Fatalf("snapshot backend called for malformed metadata: %#v", snapshots)
	}
}

func TestManagerCreatesDistinctRuntimeHandlesFromSharedCommittedParents(t *testing.T) {
	ctx := context.Background()
	store, rec, bootDigest := newReadyResolverTemplate(t, state.TemplateOriginCheckpoint, "team-a", state.TemplateBootModeResume)
	bootIndexes := &fakeBootIndexBackend{result: resolvedBootIndex(bootDigest, true, "stratovirt")}
	snapshots := &fakeSnapshotBackend{}
	manager := NewManager(store, snapshots, bootIndexes)

	first, err := manager.PrepareSandboxBoot(ctx, PrepareSandboxBootRequest{
		Namespace: "team-a", TemplateID: rec.ID, SandboxID: "sandbox-a", VMMName: "stratovirt",
	})
	if err != nil {
		t.Fatalf("first PrepareSandboxBoot() error = %v", err)
	}
	second, err := manager.PrepareSandboxBoot(ctx, PrepareSandboxBootRequest{
		Namespace: "team-a", TemplateID: rec.ID, SandboxID: "sandbox-b", VMMName: "stratovirt",
	})
	if err != nil {
		t.Fatalf("second PrepareSandboxBoot() error = %v", err)
	}

	if first.Runtime.RootfsKey == second.Runtime.RootfsKey || first.Runtime.MemKey == second.Runtime.MemKey {
		t.Fatalf("runtime handles are shared: first=%#v second=%#v", first.Runtime, second.Runtime)
	}
	if first.Runtime.RootfsMount == second.Runtime.RootfsMount || first.Runtime.MemMount == second.Runtime.MemMount || first.Runtime.VMMount == second.Runtime.VMMount {
		t.Fatalf("runtime mounts are shared: first=%#v second=%#v", first.Runtime, second.Runtime)
	}
	if len(snapshots.restores) != 2 || snapshots.restores[0].parents != snapshots.restores[1].parents {
		t.Fatalf("restore parents = %#v", snapshots.restores)
	}
	for _, call := range snapshots.restores {
		if call.memoryLayout != snapshot.MemoryLayoutCheckpointView || call.memorySizeMB != 256 {
			t.Fatalf("StratoVirt restore call = %#v", call)
		}
	}
	if first.Spec.MemoryPath != "" || first.Spec.SnapfilePath == "" || second.Spec.MemoryPath != "" || second.Spec.SnapfilePath == "" {
		t.Fatalf("StratoVirt restore specs = %#v %#v", first.Spec, second.Spec)
	}
	if len(bootIndexes.requests) != 2 {
		t.Fatalf("Boot Index resolve count = %d, want 2", len(bootIndexes.requests))
	}
}

func TestManagerRejectsNonReadyTemplateBeforeDigestResolution(t *testing.T) {
	raw := newResolverStateStore(t)
	store := NewStore(raw)
	rec, err := store.Create(context.Background(), CreateRequest{Origin: state.TemplateOriginImage})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	bootIndexes := &fakeBootIndexBackend{}
	_, err = NewManager(store, &fakeSnapshotBackend{}, bootIndexes).PrepareSandboxBoot(context.Background(), PrepareSandboxBootRequest{
		TemplateID: rec.ID,
		SandboxID:  "sandbox-a",
	})
	if err == nil || !strings.Contains(err.Error(), state.TemplateCreating) {
		t.Fatalf("PrepareSandboxBoot() error = %v, want creating error", err)
	}
	if len(bootIndexes.requests) != 0 {
		t.Fatalf("Boot Index resolver called for non-READY template: %#v", bootIndexes.requests)
	}
}

type bootIndexCall struct {
	Namespace       string
	BootIndexDigest string
}

type fakeBootIndexBackend struct {
	result   conchimage.ResolveBootIndexResult
	err      error
	requests []bootIndexCall
}

func (f *fakeBootIndexBackend) ResolveBootIndex(_ context.Context, namespace, bootIndexDigest string) (conchimage.ResolveBootIndexResult, error) {
	f.requests = append(f.requests, bootIndexCall{Namespace: namespace, BootIndexDigest: bootIndexDigest})
	if f.err != nil {
		return conchimage.ResolveBootIndexResult{}, f.err
	}
	return f.result, nil
}

type bootLayoutCall struct {
	namespace    string
	key          string
	parents      snapshot.ParentSnapshotIDs
	memoryLayout snapshot.MemoryLayoutMode
	memorySizeMB int64
}

// fakeSnapshotBackend intentionally has no SnapshotInfo method: successful
// prepare tests therefore prove Template identity no longer depends on local
// snapshot metadata probes before creating per-Sandbox handles.
type fakeSnapshotBackend struct {
	creates  []bootLayoutCall
	restores []bootLayoutCall
	releases []bootLayoutCall
}

func (f *fakeSnapshotBackend) CreateBootLayout(_ context.Context, namespace, key string, req snapshot.BootLayoutRequest) (*snapshot.BootLayout, error) {
	f.creates = append(f.creates, bootLayoutCall{
		namespace: namespace, key: key, parents: req.Parents, memoryLayout: req.MemoryLayout, memorySizeMB: req.MemorySizeMB,
	})
	return fakeBootLayout(key, req.MemorySizeMB, req.MemoryLayout), nil
}

func (f *fakeSnapshotBackend) RestoreBootLayout(_ context.Context, namespace, key string, req snapshot.BootLayoutRequest) (*snapshot.BootLayout, error) {
	f.restores = append(f.restores, bootLayoutCall{
		namespace: namespace, key: key, parents: req.Parents, memoryLayout: req.MemoryLayout, memorySizeMB: req.MemorySizeMB,
	})
	return fakeBootLayout(key, req.MemorySizeMB, req.MemoryLayout), nil
}

func (f *fakeSnapshotBackend) ReleaseBootLayout(_ context.Context, namespace, key string) error {
	f.releases = append(f.releases, bootLayoutCall{namespace: namespace, key: key})
	return nil
}

func (f *fakeSnapshotBackend) callCount() int {
	return len(f.creates) + len(f.restores) + len(f.releases)
}

func resolvedBootIndex(bootDigest string, resume bool, vmmName string) conchimage.ResolveBootIndexResult {
	result := conchimage.ResolveBootIndexResult{
		BootIndexInfo: conchimage.BootIndexInfo{
			BootIndexDigest: bootDigest,
			Resume:          resume,
			VMMName:         vmmName,
			MemorySizeMB:    256,
		},
		RootfsKey: "rootfs-committed",
		VMKey:     "vm-committed",
	}
	if resume {
		result.MemKey = "mem-committed"
	}
	return result
}

func newReadyResolverTemplate(t *testing.T, origin, namespace, mode string) (*PersistentStore, state.TemplateRecord, string) {
	t.Helper()
	store := NewStore(newResolverStateStore(t))
	rec, err := store.Create(context.Background(), CreateRequest{Origin: origin, Namespace: namespace})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	bootDigest := digest.FromString(t.Name() + "/" + origin + "/" + mode).String()
	if err := store.MarkReady(context.Background(), rec.ID, ReadyState{
		BootIndexDigest: bootDigest,
		BootMode:        mode,
	}); err != nil {
		t.Fatalf("MarkReady() error = %v", err)
	}
	return store, rec, bootDigest
}

func newResolverStateStore(t *testing.T) *state.BoltStore {
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

func assertBootIndexRequest(t *testing.T, requests []bootIndexCall, namespace, bootDigest string) {
	t.Helper()
	if len(requests) != 1 {
		t.Fatalf("ResolveBootIndex() calls = %#v, want one", requests)
	}
	if requests[0].Namespace != namespace || requests[0].BootIndexDigest != bootDigest {
		t.Fatalf("ResolveBootIndex() request = %#v", requests[0])
	}
}

func fakeBootLayout(key string, memorySizeMB int64, memoryLayout snapshot.MemoryLayoutMode) *snapshot.BootLayout {
	if memorySizeMB <= 0 {
		memorySizeMB = 256
	}
	memMount := "/mnt/" + key + "/mem"
	snapshotDir := "conch/snapshot"
	if memoryLayout == snapshot.MemoryLayoutNone {
		memMount = ""
		snapshotDir = ""
	}
	return &snapshot.BootLayout{
		RootfsMount:  "/mnt/" + key + "/rootfs",
		MemMount:     memMount,
		VMMount:      "/mnt/" + key + "/vm",
		SnapshotDir:  snapshotDir,
		MemorySizeMB: memorySizeMB,
		MemoryLayout: memoryLayout,
	}
}
