package conchbuild

import (
	"errors"
	"strings"
	"testing"

	sn "github.com/openeuler/Conch/internal/image/conchbuild/snapshot"
)

func TestResolveSnapshotComponentIDs(t *testing.T) {
	meta := &sn.SnapshotMeta{
		Labels: map[string]string{
			SnapshotLabelMemSnapshot: "mem-top",
			SnapshotLabelVMSnapshot:  "vm-top",
		},
	}

	mem, vm, err := resolveSnapshotComponentIDs(meta)
	if err != nil {
		t.Fatalf("resolveSnapshotComponentIDs: %v", err)
	}
	if mem != "mem-top" || vm != "vm-top" {
		t.Fatalf("got (%q, %q), want (%q, %q)", mem, vm, "mem-top", "vm-top")
	}
}

func TestResolveSnapshotComponentIDsRequiresLabels(t *testing.T) {
	_, _, err := resolveSnapshotComponentIDs(&sn.SnapshotMeta{
		Labels: map[string]string{
			SnapshotLabelVMSnapshot: "vm-top",
		},
	})
	if err == nil {
		t.Fatal("expected missing mem label to fail")
	}
	if !strings.Contains(err.Error(), SnapshotLabelMemSnapshot) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCollectSnapshotChainPathsWithGetter(t *testing.T) {
	chain := map[string]*sn.SnapshotMeta{
		"top":    {Parent: "mid", StoragePath: "/snap/top"},
		"mid":    {Parent: "base", StoragePath: "/snap/mid"},
		"base":   {Parent: "", StoragePath: "/snap/base"},
		"broken": {Parent: "", StoragePath: ""},
	}

	got, err := collectSnapshotChainPathsWithGetter("top", func(key string) (*sn.SnapshotMeta, error) {
		meta, ok := chain[key]
		if !ok {
			return nil, errors.New("not found")
		}
		return meta, nil
	})
	if err != nil {
		t.Fatalf("collectSnapshotChainPathsWithGetter: %v", err)
	}

	want := []string{"/snap/base", "/snap/mid", "/snap/top"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestCollectSnapshotChainPathsWithGetterRejectsEmptyStoragePath(t *testing.T) {
	_, err := collectSnapshotChainPathsWithGetter("broken", func(key string) (*sn.SnapshotMeta, error) {
		return &sn.SnapshotMeta{StoragePath: ""}, nil
	})
	if err == nil {
		t.Fatal("expected empty storage path to fail")
	}
	if !strings.Contains(err.Error(), "empty storage path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCollectSnapshotChainPathsWithGetterRejectsCycle(t *testing.T) {
	chain := map[string]*sn.SnapshotMeta{
		"top":  {Parent: "mid", StoragePath: "/snap/top"},
		"mid":  {Parent: "base", StoragePath: "/snap/mid"},
		"base": {Parent: "top", StoragePath: "/snap/base"},
	}

	_, err := collectSnapshotChainPathsWithGetter("top", func(key string) (*sn.SnapshotMeta, error) {
		meta, ok := chain[key]
		if !ok {
			return nil, errors.New("not found")
		}
		return meta, nil
	})
	if err == nil {
		t.Fatal("expected cycle to fail")
	}
	if !strings.Contains(err.Error(), "cycle detected") {
		t.Fatalf("unexpected error: %v", err)
	}
}
