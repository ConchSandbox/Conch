package containerdtemplate

import (
	"context"
	"errors"
	"testing"

	"github.com/containerd/platforms"

	conchimage "github.com/openeuler/Conch/internal/image"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

func TestStoreSameNameAcrossPlatforms(t *testing.T) {
	ctx := context.Background()
	cs := newTestContentStore(t)
	is := newMemoryImageStore()
	s := &Store{images: is, content: cs}
	const name = "registry.example/team/demo:latest"
	ids := make(map[string]string)
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		target := buildTestBootIndex(t, ctx, cs, false, platform)
		entry, err := s.Put(ctx, conchtemplate.Entry{
			Name: name, Platform: platform, BootIndexDigest: target.Digest.String(),
			Origin: conchtemplate.OriginImage, BootMode: conchtemplate.BootModeCold,
		}, target)
		if err != nil {
			t.Fatal(err)
		}
		ids[platform] = entry.BootIndexDigest
	}
	if ids["linux/amd64"] == ids["linux/arm64"] {
		t.Fatal("different platform configs must produce different IDs")
	}
	for platform, id := range ids {
		entry, err := s.Get(ctx, name, platform)
		if err != nil || entry.Name != name || entry.Platform != platform || entry.BootIndexDigest != id {
			t.Fatalf("Get(%s) = %#v, %v", platform, entry, err)
		}
	}
	entry, err := s.Get(ctx, name)
	if err != nil || entry.Platform != platforms.DefaultString() {
		t.Fatalf("default selection = %#v, %v", entry, err)
	}
	items, err := s.List(ctx, conchtemplate.Filter{})
	if err != nil || len(items) != 2 {
		t.Fatalf("List = %#v, %v", items, err)
	}
	items, err = s.List(ctx, conchtemplate.Filter{Platform: "aarch64"})
	if err != nil || len(items) != 1 || items[0].Platform != "linux/arm64" {
		t.Fatalf("filtered List = %#v, %v", items, err)
	}
	// Retarget one architecture without touching the other one.
	updated := buildTestBootIndex(t, ctx, cs, true, "linux/arm64")
	if _, err := s.Put(ctx, conchtemplate.Entry{Name: name, Platform: "linux/arm64", BootIndexDigest: updated.Digest.String(), Origin: conchtemplate.OriginCheckpoint, BootMode: conchtemplate.BootModeResume}, updated); err != nil {
		t.Fatal(err)
	}
	x86, err := s.Get(ctx, name, "x86_64")
	if err != nil || x86.BootIndexDigest != ids["linux/amd64"] {
		t.Fatalf("retarget changed x86: %#v, %v", x86, err)
	}
	if err := s.Delete(ctx, name, "aarch64"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, name, "linux/arm64"); !errors.Is(err, conchtemplate.ErrNotFound) {
		t.Fatalf("deleted ARM entry = %v", err)
	}
	if _, err := s.Get(ctx, name, "linux/amd64"); err != nil {
		t.Fatalf("delete removed x86 entry: %v", err)
	}
	if _, err := s.Get(ctx, name, "windows/amd64"); !errors.Is(err, conchtemplate.ErrInvalidArgument) {
		t.Fatalf("invalid selector = %v", err)
	}
}

func TestLegacyTemplatePlatformIsReadFromContent(t *testing.T) {
	ctx := context.Background()
	cs := newTestContentStore(t)
	is := newMemoryImageStore()
	s := &Store{images: is, content: cs}
	const name = "legacy:latest"
	x86 := buildTestBootIndex(t, ctx, cs, false, "linux/amd64")
	entry := conchtemplate.Entry{Name: name, BootIndexDigest: x86.Digest.String(), Origin: conchtemplate.OriginImage, BootMode: conchtemplate.BootModeCold}
	if _, err := s.Put(ctx, entry, x86); err != nil {
		t.Fatal(err)
	}
	key := conchimage.TemplateRecordName(name, "linux/amd64")
	legacy := is.records[key]
	delete(is.records, key)
	legacy.Name = conchimage.TemplateRecordName(name)
	delete(legacy.Labels, platformLabel)
	is.records[legacy.Name] = legacy
	if got, err := s.Get(ctx, name, "linux/amd64"); err != nil || got.Platform != "linux/amd64" {
		t.Fatalf("legacy lookup = %#v, %v", got, err)
	}
	if _, err := s.Get(ctx, name, "linux/arm64"); !errors.Is(err, conchtemplate.ErrNotFound) {
		t.Fatalf("foreign selector resolved legacy x86: %v", err)
	}
	arm := buildTestBootIndex(t, ctx, cs, false, "linux/arm64")
	entry.BootIndexDigest = arm.Digest.String()
	if _, err := s.Put(ctx, entry, arm); err != nil {
		t.Fatal(err)
	}
	entry.BootIndexDigest = x86.Digest.String()
	if _, err := s.Put(ctx, entry, x86); err != nil {
		t.Fatal(err)
	}
	items, err := s.List(ctx, conchtemplate.Filter{})
	if err != nil || len(items) != 2 {
		t.Fatalf("legacy update created duplicate or overwrote platform: %#v, %v", items, err)
	}
	if err := s.Delete(ctx, name, "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, name, "linux/arm64"); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRejectsPlatformMetadataMismatch(t *testing.T) {
	ctx := context.Background()
	cs := newTestContentStore(t)
	s := &Store{images: newMemoryImageStore(), content: cs}
	target := buildTestBootIndex(t, ctx, cs, false, "linux/amd64")
	_, err := s.Put(ctx, conchtemplate.Entry{Name: "wrong:latest", Platform: "linux/arm64", BootIndexDigest: target.Digest.String(), Origin: conchtemplate.OriginImage, BootMode: conchtemplate.BootModeCold}, target)
	if !errors.Is(err, conchtemplate.ErrInvalidArtifact) {
		t.Fatalf("platform mismatch = %v", err)
	}
}
