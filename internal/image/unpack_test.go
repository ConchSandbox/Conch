package image

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

type recordingSnapshotter struct {
	mu                sync.Mutex
	updatedInfo       snapshots.Info
	updatedFieldpaths []string
	updatedInfos      map[string]snapshots.Info
	updatedFields     map[string][]string
	updateErr         error
	statInfo          map[string]snapshots.Info
	statErr           map[string]error
	removed           []string
	removeErr         map[string]error
}

func (r *recordingSnapshotter) Stat(_ context.Context, key string) (snapshots.Info, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err, ok := r.statErr[key]; ok {
		return snapshots.Info{}, err
	}
	if info, ok := r.statInfo[key]; ok {
		return info, nil
	}
	return snapshots.Info{}, errdefs.ErrNotFound
}

func (r *recordingSnapshotter) Update(_ context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updatedInfo = info
	r.updatedFieldpaths = append([]string(nil), fieldpaths...)
	if r.updatedInfos == nil {
		r.updatedInfos = make(map[string]snapshots.Info)
	}
	if r.updatedFields == nil {
		r.updatedFields = make(map[string][]string)
	}
	r.updatedInfos[info.Name] = info
	r.updatedFields[info.Name] = append([]string(nil), fieldpaths...)
	if r.updateErr != nil {
		return snapshots.Info{}, r.updateErr
	}
	return info, nil
}

func (r *recordingSnapshotter) Usage(context.Context, string) (snapshots.Usage, error) {
	return snapshots.Usage{}, nil
}

func (r *recordingSnapshotter) Mounts(context.Context, string) ([]mount.Mount, error) {
	return nil, nil
}

func (r *recordingSnapshotter) Prepare(context.Context, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, nil
}

func (r *recordingSnapshotter) View(context.Context, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return []mount.Mount{{Type: "bind", Source: "/tmp/layer.erofs"}}, nil
}

func (r *recordingSnapshotter) Commit(context.Context, string, string, ...snapshots.Opt) error {
	return nil
}

func (r *recordingSnapshotter) Remove(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, key)
	if err := r.removeErr[key]; err != nil {
		return err
	}
	delete(r.statInfo, key)
	delete(r.statErr, key)
	return nil
}

func (r *recordingSnapshotter) Walk(context.Context, snapshots.WalkFunc, ...string) error {
	return nil
}

func (r *recordingSnapshotter) Close() error {
	return nil
}

func TestGetKindDefaultsToUnknown(t *testing.T) {
	got := getKind(ocispec.Descriptor{})
	if got != KindUnknown {
		t.Fatalf("kind: got %q want %q", got, KindUnknown)
	}
}

func TestValidateRequiredKindsRequiresRootfsAndSandbox(t *testing.T) {
	err := validateRequiredKinds(map[string]string{
		KindRootfs: "rootfs-id",
	})
	if err == nil {
		t.Fatal("expected missing sandbox kind to fail")
	}
	if !strings.Contains(err.Error(), KindSandbox) {
		t.Fatalf("unexpected error: %v", err)
	}
	if !errors.Is(err, ErrMissingSandbox) {
		t.Fatalf("error does not wrap ErrMissingSandbox: %v", err)
	}
}

func TestManifestsWithDefaultSandboxAppendsMissingSandbox(t *testing.T) {
	rootfs := ocispec.Descriptor{
		Digest:      "sha256:0000000000000000000000000000000000000000000000000000000000000001",
		Annotations: map[string]string{"io.conch.kind": KindRootfs},
	}
	defaultSandbox := ocispec.Descriptor{
		Digest:      "sha256:0000000000000000000000000000000000000000000000000000000000000002",
		Annotations: map[string]string{"io.conch.kind": KindSandbox},
	}

	got := manifestsWithDefaultSandbox([]ocispec.Descriptor{rootfs}, &defaultSandbox)
	if len(got) != 2 {
		t.Fatalf("manifest count = %d, want 2", len(got))
	}
	if getKind(got[1]) != KindSandbox {
		t.Fatalf("appended kind = %q, want %q", getKind(got[1]), KindSandbox)
	}
}

func TestManifestsWithDefaultSandboxKeepsExistingSandbox(t *testing.T) {
	rootfs := ocispec.Descriptor{
		Digest:      "sha256:0000000000000000000000000000000000000000000000000000000000000001",
		Annotations: map[string]string{"io.conch.kind": KindRootfs},
	}
	sandbox := ocispec.Descriptor{
		Digest:      "sha256:0000000000000000000000000000000000000000000000000000000000000002",
		Annotations: map[string]string{"io.conch.kind": KindSandbox},
	}
	defaultSandbox := ocispec.Descriptor{
		Digest:      "sha256:0000000000000000000000000000000000000000000000000000000000000003",
		Annotations: map[string]string{"io.conch.kind": KindSandbox},
	}

	got := manifestsWithDefaultSandbox([]ocispec.Descriptor{rootfs, sandbox}, &defaultSandbox)
	if len(got) != 2 {
		t.Fatalf("manifest count = %d, want 2", len(got))
	}
	if got[1].Digest != sandbox.Digest {
		t.Fatalf("sandbox digest = %s, want %s", got[1].Digest, sandbox.Digest)
	}
}

func TestDefaultSandboxDescriptorAnnotatesKind(t *testing.T) {
	desc := defaultSandboxDescriptor(ocispec.Descriptor{
		Digest:      "sha256:0000000000000000000000000000000000000000000000000000000000000001",
		Annotations: map[string]string{"existing": "value"},
	}, "hub.oepkgs.net/conch/kernel:6.6.0")

	if got := desc.Annotations["io.conch.kind"]; got != KindSandbox {
		t.Fatalf("kind annotation = %q, want %q", got, KindSandbox)
	}
	if got := desc.Annotations["org.opencontainers.image.ref.name"]; got != "hub.oepkgs.net/conch/kernel:6.6.0" {
		t.Fatalf("ref name = %q", got)
	}
	if got := desc.Annotations["existing"]; got != "value" {
		t.Fatalf("existing annotation = %q", got)
	}
	if desc.Digest == "" {
		t.Fatal("digest was cleared")
	}
}

func TestValidateRequiredKindsAllowsOptionalMemSnapshot(t *testing.T) {
	err := validateRequiredKinds(map[string]string{
		KindRootfs:  "rootfs-id",
		KindSandbox: "sandbox-id",
	})
	if err != nil {
		t.Fatalf("validateRequiredKinds: %v", err)
	}
}

func TestValidateBootIndexManifestKindsRejectsDuplicateAndUnknownKinds(t *testing.T) {
	descriptor := func(kind, payload string) ocispec.Descriptor {
		return ocispec.Descriptor{
			MediaType:   ocispec.MediaTypeImageManifest,
			Digest:      digest.FromString(payload),
			Size:        1,
			Annotations: map[string]string{"io.conch.kind": kind},
		}
	}

	_, err := validateBootIndexManifestKinds([]ocispec.Descriptor{
		descriptor(KindRootfs, "rootfs-a"),
		descriptor(KindRootfs, "rootfs-b"),
		descriptor(KindSandbox, "sandbox"),
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate kind error = %v", err)
	}

	_, err = validateBootIndexManifestKinds([]ocispec.Descriptor{
		descriptor(KindRootfs, "rootfs"),
		descriptor(KindUnknown, "unknown"),
		descriptor(KindSandbox, "sandbox"),
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unknown kind error = %v", err)
	}
}

func TestRecordRootfsSnapshotProvenance(t *testing.T) {
	snapshotter := &recordingSnapshotter{}

	err := recordRootfsSnapshotProvenance(context.Background(), snapshotter, map[string]string{
		KindRootfs:      "rootfs-id",
		KindSandbox:     "sandbox-id",
		KindMemSnapshot: "mem-id",
	}, "localhost/conch/rootfs-component:abc", "sha256:abc")
	if err != nil {
		t.Fatalf("linkSnapshotLabels: %v", err)
	}
	rootfsInfo := snapshotter.updatedInfos["rootfs-id"]
	if rootfsInfo.Name != "rootfs-id" {
		t.Fatalf("updated rootfs snapshot: got %q want %q", rootfsInfo.Name, "rootfs-id")
	}
	if rootfsInfo.Labels[common.SnapshotLabelRootfsImage] != "localhost/conch/rootfs-component:abc" {
		t.Fatalf("rootfs image label: got %q", rootfsInfo.Labels[common.SnapshotLabelRootfsImage])
	}
	if _, ok := snapshotter.updatedInfos["sandbox-id"]; ok {
		t.Fatal("sandbox snapshot should not receive relationship labels")
	}
	if _, ok := snapshotter.updatedInfos["mem-id"]; ok {
		t.Fatal("mem snapshot should not receive relationship labels")
	}
}

func TestRecordRootfsSnapshotProvenanceRequiresRootfs(t *testing.T) {
	err := recordRootfsSnapshotProvenance(context.Background(), &recordingSnapshotter{}, map[string]string{
		KindSandbox: "sandbox-id",
	}, "", "")
	if err == nil {
		t.Fatal("expected missing rootfs kind to fail")
	}
	if !strings.Contains(err.Error(), "rootfs") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRecordRootfsSnapshotProvenanceWrapsUpdateError(t *testing.T) {
	snapshotter := &recordingSnapshotter{updateErr: errors.New("boom")}

	err := recordRootfsSnapshotProvenance(context.Background(), snapshotter, map[string]string{
		KindRootfs:  "rootfs-id",
		KindSandbox: "sandbox-id",
	}, "rootfs-image", "")
	if err == nil {
		t.Fatal("expected update error")
	}
	if !strings.Contains(err.Error(), "failed to record rootfs snapshot provenance") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSnapshotExists(t *testing.T) {
	snapshotter := &recordingSnapshotter{
		statInfo: map[string]snapshots.Info{
			"existing": {Name: "existing"},
		},
	}

	exists, err := snapshotExists(context.Background(), snapshotter, "existing")
	if err != nil {
		t.Fatalf("snapshotExists(existing): %v", err)
	}
	if !exists {
		t.Fatal("expected existing snapshot to be reported as present")
	}

	exists, err = snapshotExists(context.Background(), snapshotter, "missing")
	if err != nil {
		t.Fatalf("snapshotExists(missing): %v", err)
	}
	if exists {
		t.Fatal("expected missing snapshot to be reported as absent")
	}
}

func TestRecordCreatedSnapshotOnlyTracksNewSnapshots(t *testing.T) {
	var created []createdSnapshot
	snapshotter := &recordingSnapshotter{}

	recordCreatedSnapshot(&created, snapshotter, "existing", true)
	recordCreatedSnapshot(&created, snapshotter, "newly-created", false)

	if len(created) != 1 || created[0].key != "newly-created" {
		t.Fatalf("created snapshots = %#v, want newly-created", created)
	}
}

func TestCleanupSnapshotsRemovesRecordedSnapshots(t *testing.T) {
	snapshotter := &recordingSnapshotter{}

	cleanupSnapshots([]createdSnapshot{
		{key: "snap-a", snapshotter: snapshotter},
		{key: "snap-b", snapshotter: snapshotter},
	}, context.Background())

	if got, want := strings.Join(snapshotter.removed, "\x00"), strings.Join([]string{"snap-a", "snap-b"}, "\x00"); got != want {
		t.Fatalf("removed snapshots = %#v, want %#v", snapshotter.removed, []string{"snap-a", "snap-b"})
	}
}

func TestKeyedUnpackLocksSerializeSameKeyAndReleaseEntries(t *testing.T) {
	var locks keyedUnpackLocks
	var active atomic.Int32
	var maximum atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})

	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := locks.acquire(context.Background(), "namespace\x00boot-index")
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			current := active.Add(1)
			for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			release()
		}()
	}
	close(start)
	wg.Wait()

	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent holders = %d, want 1", got)
	}
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.entries) != 0 {
		t.Fatalf("lock entries leaked: %d", len(locks.entries))
	}
}

func TestKeyedUnpackLocksHonorCancellation(t *testing.T) {
	var locks keyedUnpackLocks
	release, err := locks.acquire(context.Background(), "same-key")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := locks.acquire(ctx, "same-key"); !errors.Is(err, context.Canceled) {
		t.Fatalf("second acquire error = %v, want context.Canceled", err)
	}
	release()

	locks.mu.Lock()
	defer locks.mu.Unlock()
	if len(locks.entries) != 0 {
		t.Fatalf("lock entries leaked after cancellation: %d", len(locks.entries))
	}
}

func TestEnsureSnapshotChainUnpackedRecreatesMissingCommittedSnapshot(t *testing.T) {
	diffIDs, chainID := singleLayerSnapshotChain("missing-layer")
	snapshotter := &recordingSnapshotter{statInfo: make(map[string]snapshots.Info)}
	var created []createdSnapshot
	unpackCalls := 0

	err := ensureSnapshotChainUnpacked(context.Background(), snapshotter, diffIDs, func() error {
		unpackCalls++
		snapshotter.mu.Lock()
		snapshotter.statInfo[chainID] = snapshots.Info{Name: chainID, Kind: snapshots.KindCommitted}
		snapshotter.mu.Unlock()
		return nil
	}, &created)
	if err != nil {
		t.Fatalf("ensureSnapshotChainUnpacked: %v", err)
	}
	if unpackCalls != 1 {
		t.Fatalf("unpack calls = %d, want 1", unpackCalls)
	}
	if len(created) != 1 || created[0].key != chainID {
		t.Fatalf("created snapshots = %#v, want %s", created, chainID)
	}
}

func TestEnsureSnapshotChainUnpackedSkipsHealthyCommittedSnapshot(t *testing.T) {
	diffIDs, chainID := singleLayerSnapshotChain("healthy-layer")
	snapshotter := &recordingSnapshotter{statInfo: map[string]snapshots.Info{
		chainID: {Name: chainID, Kind: snapshots.KindCommitted},
	}}
	var created []createdSnapshot
	unpackCalls := 0

	err := ensureSnapshotChainUnpacked(context.Background(), snapshotter, diffIDs, func() error {
		unpackCalls++
		return nil
	}, &created)
	if err != nil {
		t.Fatalf("ensureSnapshotChainUnpacked: %v", err)
	}
	if unpackCalls != 0 {
		t.Fatalf("unpack calls = %d, want 0", unpackCalls)
	}
	if len(created) != 0 {
		t.Fatalf("healthy pre-existing snapshot recorded as created: %#v", created)
	}
}

func TestEnsureSnapshotChainUnpackedRebuildsCorruptParentSuffix(t *testing.T) {
	diffIDs := []digest.Digest{
		digest.FromString("base-layer"),
		digest.FromString("middle-layer"),
		digest.FromString("top-layer"),
	}
	baseID := identity.ChainID(diffIDs[:1]).String()
	middleID := identity.ChainID(diffIDs[:2]).String()
	topID := identity.ChainID(diffIDs).String()
	snapshotter := &recordingSnapshotter{statInfo: map[string]snapshots.Info{
		baseID:   {Name: baseID, Kind: snapshots.KindCommitted},
		middleID: {Name: middleID, Parent: "wrong-parent", Kind: snapshots.KindCommitted},
		topID:    {Name: topID, Parent: middleID, Kind: snapshots.KindCommitted},
	}}
	var created []createdSnapshot
	unpackCalls := 0

	err := ensureSnapshotChainUnpacked(context.Background(), snapshotter, diffIDs, func() error {
		unpackCalls++
		snapshotter.mu.Lock()
		snapshotter.statInfo[middleID] = snapshots.Info{Name: middleID, Parent: baseID, Kind: snapshots.KindCommitted}
		snapshotter.statInfo[topID] = snapshots.Info{Name: topID, Parent: middleID, Kind: snapshots.KindCommitted}
		snapshotter.mu.Unlock()
		return nil
	}, &created)
	if err != nil {
		t.Fatalf("ensureSnapshotChainUnpacked: %v", err)
	}
	if unpackCalls != 1 {
		t.Fatalf("unpack calls = %d, want 1", unpackCalls)
	}
	var rebuildRemovals []string
	for _, key := range snapshotter.removed {
		if key == topID || key == middleID || key == baseID {
			rebuildRemovals = append(rebuildRemovals, key)
		}
	}
	if got, want := rebuildRemovals, []string{topID, middleID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rebuild removals = %#v, want %#v", got, want)
	}
	if containsString(snapshotter.removed, baseID) {
		t.Fatalf("healthy base snapshot was removed: %#v", snapshotter.removed)
	}
	if len(created) != 2 || created[0].key != topID || created[1].key != middleID {
		t.Fatalf("created snapshots = %#v, want rebuilt suffix in child-first cleanup order", created)
	}
}

func TestEnsureSnapshotChainUnpackedDoesNotDeleteUnexpectedSnapshotKind(t *testing.T) {
	diffIDs, chainID := singleLayerSnapshotChain("active-collision-layer")
	snapshotter := &recordingSnapshotter{statInfo: map[string]snapshots.Info{
		chainID: {Name: chainID, Kind: snapshots.KindActive},
	}}

	err := ensureSnapshotChainUnpacked(context.Background(), snapshotter, diffIDs, func() error {
		t.Fatal("unpack must not run for an active snapshot name collision")
		return nil
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "want committed") {
		t.Fatalf("error = %v, want committed-kind validation error", err)
	}
	if containsString(snapshotter.removed, chainID) {
		t.Fatalf("active snapshot was removed: %#v", snapshotter.removed)
	}
}

func singleLayerSnapshotChain(value string) ([]digest.Digest, string) {
	diffIDs := []digest.Digest{digest.FromString(value)}
	return diffIDs, identity.ChainID(diffIDs).String()
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
