package image

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type recordingSnapshotter struct {
	updatedInfo       snapshots.Info
	updatedFieldpaths []string
	updateErr         error
}

func (r *recordingSnapshotter) Stat(context.Context, string) (snapshots.Info, error) {
	return snapshots.Info{}, nil
}

func (r *recordingSnapshotter) Update(_ context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	r.updatedInfo = info
	r.updatedFieldpaths = append([]string(nil), fieldpaths...)
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

func (r *recordingSnapshotter) Remove(context.Context, string) error {
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
	})
	if err != nil {
		t.Fatalf("linkSnapshotLabels: %v", err)
	}
	if snapshotter.updatedInfo.Name != "rootfs-id" {
		t.Fatalf("updated snapshot: got %q want %q", snapshotter.updatedInfo.Name, "rootfs-id")
	}
	if snapshotter.updatedInfo.Labels[SnapshotLabelVMSnapshot] != "sandbox-id" {
		t.Fatalf("sandbox label: got %q want %q", snapshotter.updatedInfo.Labels[SnapshotLabelVMSnapshot], "sandbox-id")
	}
	if snapshotter.updatedInfo.Labels[SnapshotLabelMemSnapshot] != "mem-id" {
		t.Fatalf("mem label: got %q want %q", snapshotter.updatedInfo.Labels[SnapshotLabelMemSnapshot], "mem-id")
	}
}

func TestLinkSnapshotLabelsRequiresRootfsAndSandbox(t *testing.T) {
	err := linkSnapshotLabels(context.Background(), &recordingSnapshotter{}, map[string]string{
		KindRootfs: "rootfs-id",
	})
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
	})
	if err == nil {
		t.Fatal("expected update error")
	}
	if !strings.Contains(err.Error(), "failed to link component SnapshotIDs to rootfs") {
		t.Fatalf("unexpected error: %v", err)
	}
}
