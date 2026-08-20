package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"

	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/snapshot"
	"github.com/openeuler/Conch/internal/snapshot/common"
	"github.com/openeuler/Conch/internal/template"
)

func TestBootPreparerColdCreateResolvesBootIndexWithoutSnapshotInfo(t *testing.T) {
	ctx := context.Background()
	templates, entry, bootDigest := newBootTemplate(t, template.OriginImage, template.BootModeCold)
	resolver := &fakeBootResolver{result: resolvedBoot(bootDigest, false, "")}
	snapshots := &fakeSnapshotBackend{}
	preparer := mustBootPreparer(t, templates, snapshots, resolver)

	got, err := prepareBootForTest(ctx, preparer, bootTestRequest{
		TemplateID: entry.BootIndexDigest,
		SandboxID:  "sandbox-a",
		VMMName:    "cloud-hypervisor",
		RAMMB:      512,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	assertBootResolverRequest(t, resolver, bootDigest)
	if len(snapshots.creates) != 1 || len(snapshots.restores) != 0 {
		t.Fatalf("snapshot calls: creates=%#v restores=%#v", snapshots.creates, snapshots.restores)
	}
	call := snapshots.creates[0]
	if call.key != "sandbox-a" || call.memorySizeMB != 512 {
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

func TestBootPreparerStratovirtColdCreateUsesNoMemoryLayer(t *testing.T) {
	ctx := context.Background()
	templates, entry, bootDigest := newBootTemplate(t, template.OriginImage, template.BootModeCold)
	resolver := &fakeBootResolver{result: resolvedBoot(bootDigest, false, "")}
	snapshots := &fakeSnapshotBackend{}

	got, err := prepareBootForTest(ctx, mustBootPreparer(t, templates, snapshots, resolver), bootTestRequest{
		TemplateID: entry.BootIndexDigest,
		SandboxID:  "sandbox-stratovirt",
		VMMName:    "stratovirt",
		RAMMB:      768,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
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
	if got.Runtime.MemKey != "" || got.Runtime.MemorySnapshotRoot != "" {
		t.Fatalf("StratoVirt cold runtime = %#v", got.Runtime)
	}
}

func TestBootPreparerResumeRestoresResolvedBootIndex(t *testing.T) {
	ctx := context.Background()
	templates, entry, bootDigest := newBootTemplate(t, template.OriginCheckpoint, template.BootModeResume)
	resolver := &fakeBootResolver{result: resolvedBoot(bootDigest, true, "cloud-hypervisor")}
	snapshots := &fakeSnapshotBackend{}
	preparer := mustBootPreparer(t, templates, snapshots, resolver)

	got, err := prepareBootForTest(ctx, preparer, bootTestRequest{
		TemplateID: entry.BootIndexDigest,
		SandboxID:  "sandbox-a",
		VMMName:    "cloud-hypervisor",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	assertBootResolverRequest(t, resolver, bootDigest)
	if len(snapshots.restores) != 1 || len(snapshots.creates) != 0 {
		t.Fatalf("snapshot calls: creates=%#v restores=%#v", snapshots.creates, snapshots.restores)
	}
	call := snapshots.restores[0]
	if call.key != "sandbox-a" {
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
	if want := filepath.Join(got.Runtime.MemorySnapshotRoot, common.VMMStateDir); got.Spec.SnapfilePath != want {
		t.Fatalf("resume state path = %q, want %q", got.Spec.SnapfilePath, want)
	}
}

func TestBootPreparerStratovirtResumePropagatesIncrementalMemoryFormat(t *testing.T) {
	templates, entry, bootDigest := newBootTemplate(t, template.OriginCheckpoint, template.BootModeResume)
	resolved := resolvedBoot(bootDigest, true, "stratovirt")
	resolved.MemoryFormat = conchimage.MemoryFormatIncrementalV1
	snapshots := &fakeSnapshotBackend{}

	got, err := prepareBootForTest(context.Background(), mustBootPreparer(t, templates, snapshots, &fakeBootResolver{result: resolved}), bootTestRequest{
		TemplateID:          entry.BootIndexDigest,
		SandboxID:           "sandbox-incremental",
		VMMName:             "stratovirt",
		RequestedMemoryMode: "incremental",
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(snapshots.restores) != 1 || snapshots.restores[0].memoryFormat != conchimage.MemoryFormatIncrementalV1 {
		t.Fatalf("restore calls = %#v", snapshots.restores)
	}
	if got.Spec.MemoryFormat != conchimage.MemoryFormatIncrementalV1 || got.Runtime.MemoryFormat != conchimage.MemoryFormatIncrementalV1 {
		t.Fatalf("prepared boot = %#v", got)
	}
	if want := filepath.Join(got.Runtime.MemorySnapshotRoot, common.VMMStateDir); got.Spec.SnapfilePath != want {
		t.Fatalf("incremental state path = %q, want %q", got.Spec.SnapfilePath, want)
	}
}

func TestBootPreflightRejectsIncompatibleMemoryModeBeforeUnpack(t *testing.T) {
	templates, entry, bootDigest := newBootTemplate(t, template.OriginCheckpoint, template.BootModeResume)
	resolved := resolvedBoot(bootDigest, true, "stratovirt")
	resolved.MemoryFormat = conchimage.MemoryFormatIncrementalV1
	resolver := &fakeBootResolver{result: resolved}
	snapshots := &fakeSnapshotBackend{}
	preparer := mustBootPreparer(t, templates, snapshots, resolver)

	_, err := preparer.Preflight(context.Background(), BootPreflightRequest{
		TemplateID:          entry.BootIndexDigest,
		VMMName:             "stratovirt",
		RequestedMemoryMode: "full",
	})
	if !errors.Is(err, ErrFailedPrecondition) {
		t.Fatalf("Preflight() error = %v, want failed precondition", err)
	}
	if resolver.resolveCalls != 0 || snapshots.callCount() != 0 {
		t.Fatalf("preflight allocated boot resources: resolve=%d snapshots=%d", resolver.resolveCalls, snapshots.callCount())
	}
}

func TestBootPreparerRejectsMissingBootIndexDigest(t *testing.T) {
	entry := template.Entry{
		Origin:   template.OriginImage,
		BootMode: template.BootModeCold,
	}
	templates := &fakeTemplateReader{entry: entry}
	resolver := &fakeBootResolver{}
	snapshots := &fakeSnapshotBackend{}

	_, err := prepareBootForTest(context.Background(), mustBootPreparer(t, templates, snapshots, resolver), bootTestRequest{
		TemplateID: digest.FromString("missing").String(),
		SandboxID:  "sandbox-a",
	})
	if err == nil || !strings.Contains(err.Error(), "has no boot index digest") {
		t.Fatalf("Prepare() error = %v, want missing digest error", err)
	}
	if len(resolver.requests) != 0 || snapshots.callCount() != 0 {
		t.Fatalf("backends called for missing digest: resolver=%#v snapshots=%#v", resolver.requests, snapshots)
	}
}

func TestBootPreparerRejectsCachedCapabilityMismatch(t *testing.T) {
	for _, tt := range []struct {
		name         string
		cachedMode   template.BootMode
		resolvedMode template.BootMode
	}{
		{name: "cached cold resolved resume", cachedMode: template.BootModeCold, resolvedMode: template.BootModeResume},
		{name: "cached resume resolved cold", cachedMode: template.BootModeResume, resolvedMode: template.BootModeCold},
	} {
		t.Run(tt.name, func(t *testing.T) {
			templates, entry, bootDigest := newBootTemplate(t, template.OriginImage, tt.cachedMode)
			resume := tt.resolvedMode == template.BootModeResume
			vmmName := ""
			if resume {
				vmmName = "cloud-hypervisor"
			}
			resolver := &fakeBootResolver{result: resolvedBoot(bootDigest, resume, vmmName)}
			snapshots := &fakeSnapshotBackend{}
			_, err := prepareBootForTest(context.Background(), mustBootPreparer(t, templates, snapshots, resolver), bootTestRequest{
				TemplateID: entry.BootIndexDigest,
				SandboxID:  "sandbox-a",
			})
			if err == nil || !strings.Contains(err.Error(), "cached boot mode") {
				t.Fatalf("Prepare() error = %v, want capability mismatch", err)
			}
			if snapshots.callCount() != 0 {
				t.Fatalf("snapshot backend called for mismatched capability: %#v", snapshots)
			}
		})
	}
}

func TestBootPreparerRejectsResumeVMMMismatch(t *testing.T) {
	templates, entry, bootDigest := newBootTemplate(t, template.OriginCheckpoint, template.BootModeResume)
	resolver := &fakeBootResolver{result: resolvedBoot(bootDigest, true, "cloud-hypervisor")}
	snapshots := &fakeSnapshotBackend{}

	_, err := prepareBootForTest(context.Background(), mustBootPreparer(t, templates, snapshots, resolver), bootTestRequest{
		TemplateID: entry.BootIndexDigest,
		SandboxID:  "sandbox-a",
		VMMName:    "stratovirt",
	})
	if err == nil || !strings.Contains(err.Error(), "captured by VMM cloud-hypervisor, not stratovirt") {
		t.Fatalf("Prepare() error = %v, want VMM mismatch", err)
	}
	if snapshots.callCount() != 0 {
		t.Fatalf("snapshot backend called for VMM mismatch: %#v", snapshots)
	}
}

func TestBootPreparerRejectsStratovirtResumeWithoutMemorySize(t *testing.T) {
	templates, entry, bootDigest := newBootTemplate(t, template.OriginCheckpoint, template.BootModeResume)
	resolved := resolvedBoot(bootDigest, true, "stratovirt")
	resolved.MemorySizeMB = 0
	snapshots := &fakeSnapshotBackend{}

	_, err := prepareBootForTest(context.Background(), mustBootPreparer(t, templates, snapshots, &fakeBootResolver{result: resolved}), bootTestRequest{
		TemplateID: entry.BootIndexDigest,
		SandboxID:  "sandbox-a",
		VMMName:    "stratovirt",
	})
	if err == nil || !strings.Contains(err.Error(), "missing memory size") {
		t.Fatalf("Prepare() error = %v", err)
	}
	if snapshots.callCount() != 0 {
		t.Fatalf("snapshot backend called for malformed metadata: %#v", snapshots)
	}
}

func TestBootPreparerCreatesDistinctRuntimeHandlesFromSharedCommittedParents(t *testing.T) {
	ctx := context.Background()
	templates, entry, bootDigest := newBootTemplate(t, template.OriginCheckpoint, template.BootModeResume)
	resolver := &fakeBootResolver{result: resolvedBoot(bootDigest, true, "stratovirt")}
	snapshots := &fakeSnapshotBackend{}
	preparer := mustBootPreparer(t, templates, snapshots, resolver)

	first, err := prepareBootForTest(ctx, preparer, bootTestRequest{
		TemplateID: entry.BootIndexDigest, SandboxID: "sandbox-a", VMMName: "stratovirt",
	})
	if err != nil {
		t.Fatalf("first Prepare() error = %v", err)
	}
	second, err := prepareBootForTest(ctx, preparer, bootTestRequest{
		TemplateID: entry.BootIndexDigest, SandboxID: "sandbox-b", VMMName: "stratovirt",
	})
	if err != nil {
		t.Fatalf("second Prepare() error = %v", err)
	}

	if first.Runtime.RootfsKey == second.Runtime.RootfsKey || first.Runtime.MemKey == second.Runtime.MemKey {
		t.Fatalf("runtime handles are shared: first=%#v second=%#v", first.Runtime, second.Runtime)
	}
	if first.Runtime.RootfsMount == second.Runtime.RootfsMount ||
		first.Runtime.MemorySnapshotRoot == second.Runtime.MemorySnapshotRoot ||
		first.Runtime.VMMount == second.Runtime.VMMount {
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
	if first.Spec.MemoryPath != "" || first.Spec.SnapfilePath == "" ||
		second.Spec.MemoryPath != "" || second.Spec.SnapfilePath == "" {
		t.Fatalf("StratoVirt restore specs = %#v %#v", first.Spec, second.Spec)
	}
	if len(resolver.requests) != 2 {
		t.Fatalf("Boot resolver count = %d, want 2", len(resolver.requests))
	}
}

type fakeTemplateReader struct {
	entry template.Entry
	err   error
}

func (f *fakeTemplateReader) Get(_ context.Context, bootIndexDigest string) (template.Entry, error) {
	if f.err != nil {
		return template.Entry{}, f.err
	}
	if f.entry.BootIndexDigest != "" && f.entry.BootIndexDigest != bootIndexDigest {
		return template.Entry{}, fmt.Errorf("template %s not found", bootIndexDigest)
	}
	return f.entry, nil
}

type bootResolverCall struct {
	BootIndexDigest string
}

type fakeBootResolver struct {
	result       conchimage.ResolvedBoot
	err          error
	requests     []bootResolverCall
	resolveCalls int
}

func (f *fakeBootResolver) InspectBoot(_ context.Context, bootIndexDigest string) (conchimage.BootIndexInfo, error) {
	f.requests = append(f.requests, bootResolverCall{BootIndexDigest: bootIndexDigest})
	if f.err != nil {
		return conchimage.BootIndexInfo{}, f.err
	}
	return conchimage.BootIndexInfo{
		BootIndexDigest: f.result.BootIndexDigest,
		Resume:          f.result.Resume,
		VMMName:         f.result.VMMName,
		MemorySizeMB:    f.result.MemorySizeMB,
		MemoryFormat:    f.result.MemoryFormat,
	}, nil
}

func (f *fakeBootResolver) ResolveBootFromInfo(_ context.Context, _ conchimage.BootIndexInfo) (conchimage.ResolvedBoot, error) {
	f.resolveCalls++
	return f.result, nil
}

type bootLayoutCall struct {
	key          string
	parents      snapshot.ParentSnapshotIDs
	memoryLayout snapshot.MemoryLayoutMode
	memorySizeMB int64
	memoryFormat string
}

// fakeSnapshotBackend intentionally has no snapshot metadata query method:
// successful prepare tests prove boot identity is resolved from immutable
// Template and Boot Index data only.
type fakeSnapshotBackend struct {
	creates  []bootLayoutCall
	restores []bootLayoutCall
	releases []bootLayoutCall
}

func (f *fakeSnapshotBackend) CreateBootLayout(_ context.Context, key string, req snapshot.BootLayoutRequest) (*snapshot.BootLayout, error) {
	f.creates = append(f.creates, bootLayoutCall{
		key:          key,
		parents:      req.Parents,
		memoryLayout: req.MemoryLayout,
		memorySizeMB: req.MemorySizeMB,
		memoryFormat: req.MemoryFormat,
	})
	return fakeBootLayout(key, req.MemorySizeMB, req.MemoryLayout, req.MemoryFormat), nil
}

func (f *fakeSnapshotBackend) RestoreBootLayout(_ context.Context, key string, req snapshot.BootLayoutRequest) (*snapshot.BootLayout, error) {
	f.restores = append(f.restores, bootLayoutCall{
		key:          key,
		parents:      req.Parents,
		memoryLayout: req.MemoryLayout,
		memorySizeMB: req.MemorySizeMB,
		memoryFormat: req.MemoryFormat,
	})
	return fakeBootLayout(key, req.MemorySizeMB, req.MemoryLayout, req.MemoryFormat), nil
}

func (f *fakeSnapshotBackend) ReleaseBootLayout(_ context.Context, key string) error {
	f.releases = append(f.releases, bootLayoutCall{key: key})
	return nil
}

func (f *fakeSnapshotBackend) callCount() int {
	return len(f.creates) + len(f.restores) + len(f.releases)
}

func resolvedBoot(bootDigest string, resume bool, vmmName string) conchimage.ResolvedBoot {
	result := conchimage.ResolvedBoot{
		BootIndexDigest: bootDigest,
		RootfsKey:       "rootfs-committed",
		VMKey:           "vm-committed",
		Resume:          resume,
		VMMName:         vmmName,
		MemorySizeMB:    256,
	}
	if resume {
		result.MemKey = "mem-committed"
		if vmmName == "stratovirt" {
			result.MemoryFormat = conchimage.MemoryFormatFull
		}
	}
	return result
}

func newBootTemplate(t *testing.T, origin template.Origin, mode template.BootMode) (*fakeTemplateReader, template.Entry, string) {
	t.Helper()
	bootDigest := digest.FromString(t.Name() + "/" + string(origin) + "/" + string(mode)).String()
	entry := template.Entry{
		Origin:          origin,
		BootMode:        mode,
		BootIndexDigest: bootDigest,
	}
	return &fakeTemplateReader{entry: entry}, entry, bootDigest
}

func mustBootPreparer(t *testing.T, templates TemplateReader, snapshots SnapshotBackend, resolver *fakeBootResolver) BootPreparer {
	t.Helper()
	preparer, err := newBootPreparer(templates, snapshots, resolver.InspectBoot, resolver.ResolveBootFromInfo)
	if err != nil {
		t.Fatalf("NewBootPreparer() error = %v", err)
	}
	return preparer
}

type bootTestRequest struct {
	TemplateID          string
	SandboxID           string
	VMMName             string
	RAMMB               int64
	RequestedMemoryMode string
}

func prepareBootForTest(ctx context.Context, preparer BootPreparer, req bootTestRequest) (PreparedBoot, error) {
	plan, err := preparer.Preflight(ctx, BootPreflightRequest{
		TemplateID:          req.TemplateID,
		VMMName:             req.VMMName,
		RequestedMemoryMode: req.RequestedMemoryMode,
	})
	if err != nil {
		return PreparedBoot{}, err
	}
	return preparer.Prepare(ctx, PrepareBootRequest{
		Plan:      plan,
		SandboxID: req.SandboxID,
		RAMMB:     req.RAMMB,
	})
}

func assertBootResolverRequest(t *testing.T, resolver *fakeBootResolver, bootDigest string) {
	t.Helper()
	if len(resolver.requests) != 1 || resolver.resolveCalls != 1 {
		t.Fatalf("boot resolution calls: inspect=%#v unpack=%d, want one each", resolver.requests, resolver.resolveCalls)
	}
	if resolver.requests[0].BootIndexDigest != bootDigest {
		t.Fatalf("InspectBootIndex() request = %#v", resolver.requests[0])
	}
}

func fakeBootLayout(key string, memorySizeMB int64, memoryLayout snapshot.MemoryLayoutMode, memoryFormat string) *snapshot.BootLayout {
	if memorySizeMB <= 0 {
		memorySizeMB = 256
	}
	memorySnapshotRoot := "/mnt/" + key + "/mem"
	if memoryLayout == snapshot.MemoryLayoutNone {
		memorySnapshotRoot = ""
	}
	return &snapshot.BootLayout{
		RootfsMount:        "/mnt/" + key + "/rootfs",
		MemorySnapshotRoot: memorySnapshotRoot,
		VMMount:            "/mnt/" + key + "/vm",
		MemorySizeMB:       memorySizeMB,
		MemoryLayout:       memoryLayout,
		MemoryFormat:       memoryFormat,
	}
}
