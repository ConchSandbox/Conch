package image

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

type recordingSnapshotter struct {
	updatedInfo       snapshots.Info
	updatedFieldpaths []string
	updatedInfos      map[string]snapshots.Info
	updatedFields     map[string][]string
	updateErr         error
	statInfo          map[string]snapshots.Info
	statErr           map[string]error
	removed           []string
}

func (r *recordingSnapshotter) Stat(_ context.Context, key string) (snapshots.Info, error) {
	if err, ok := r.statErr[key]; ok {
		return snapshots.Info{}, err
	}
	if info, ok := r.statInfo[key]; ok {
		return info, nil
	}
	return snapshots.Info{}, errdefs.ErrNotFound
}

func (r *recordingSnapshotter) Update(_ context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
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
	return nil, nil
}

func (r *recordingSnapshotter) Commit(context.Context, string, string, ...snapshots.Opt) error {
	return nil
}

func (r *recordingSnapshotter) Remove(_ context.Context, key string) error {
	r.removed = append(r.removed, key)
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

func TestLinkSnapshotLabelsLinksSandboxAndOptionalMem(t *testing.T) {
	snapshotter := &recordingSnapshotter{}

	err := linkSnapshotLabels(context.Background(), snapshotter, map[string]string{
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
	if rootfsInfo.Labels[common.SnapshotLabelGroupVMRef] != "sandbox-id" {
		t.Fatalf("sandbox label: got %q want %q", rootfsInfo.Labels[common.SnapshotLabelGroupVMRef], "sandbox-id")
	}
	if rootfsInfo.Labels[common.SnapshotLabelGroupMemRef] != "mem-id" {
		t.Fatalf("mem label: got %q want %q", rootfsInfo.Labels[common.SnapshotLabelGroupMemRef], "mem-id")
	}
	if rootfsInfo.Labels[common.SnapshotLabelRootfsImage] != "localhost/conch/rootfs-component:abc" {
		t.Fatalf("rootfs image label: got %q", rootfsInfo.Labels[common.SnapshotLabelRootfsImage])
	}
	sandboxInfo := snapshotter.updatedInfos["sandbox-id"]
	if sandboxInfo.Labels[common.SnapshotLabelGroupID] != "rootfs-id" {
		t.Fatalf("sandbox group id label: got %q want rootfs-id", sandboxInfo.Labels[common.SnapshotLabelGroupID])
	}
	if sandboxInfo.Labels[common.SnapshotLabelComponentKind] != common.SnapshotComponentKindVM {
		t.Fatalf("sandbox component kind = %q, want %q", sandboxInfo.Labels[common.SnapshotLabelComponentKind], common.SnapshotComponentKindVM)
	}
	memInfo := snapshotter.updatedInfos["mem-id"]
	if memInfo.Labels[common.SnapshotLabelGroupID] != "rootfs-id" {
		t.Fatalf("mem group id label: got %q want rootfs-id", memInfo.Labels[common.SnapshotLabelGroupID])
	}
	if memInfo.Labels[common.SnapshotLabelComponentKind] != common.SnapshotComponentKindMem {
		t.Fatalf("mem component kind = %q, want %q", memInfo.Labels[common.SnapshotLabelComponentKind], common.SnapshotComponentKindMem)
	}
}

func TestLinkSnapshotLabelsRequiresRootfsAndSandbox(t *testing.T) {
	err := linkSnapshotLabels(context.Background(), &recordingSnapshotter{}, map[string]string{
		KindRootfs: "rootfs-id",
	}, "", "")
	if err == nil {
		t.Fatal("expected missing sandbox kind to fail")
	}
	if !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLinkSnapshotLabelsWrapsUpdateError(t *testing.T) {
	snapshotter := &recordingSnapshotter{updateErr: errors.New("boom")}

	err := linkSnapshotLabels(context.Background(), snapshotter, map[string]string{
		KindRootfs:  "rootfs-id",
		KindSandbox: "sandbox-id",
	}, "", "")
	if err == nil {
		t.Fatal("expected update error")
	}
	if !strings.Contains(err.Error(), "failed to link component SnapshotIDs to rootfs") {
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
