package image

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func platformTestComponent(t *testing.T, store content.Store, kind, platform string) ocispec.Descriptor {
	t.Helper()
	path := filepath.Join(t.TempDir(), "layer.erofs")
	if err := os.WriteFile(path, []byte(kind), 0o600); err != nil {
		t.Fatal(err)
	}
	desc, err := BuildNativeComponentInContent(context.Background(), store, []string{path}, kind, platform)
	if err != nil {
		t.Fatal(err)
	}
	return desc
}

func platformTestIndex(t *testing.T, store content.Store, platform string) ocispec.Descriptor {
	t.Helper()
	target, err := BuildBootIndexInContent(context.Background(), store, BootIndexContentOptions{
		Platform:          platform,
		RootfsDescriptor:  platformTestComponent(t, store, KindRootfs, platform),
		SandboxDescriptor: platformTestComponent(t, store, KindSandbox, platform),
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := platforms.Parse(platform)
	if err != nil {
		t.Fatal(err)
	}
	target.Platform = &p
	return target
}

func platformTestStore(t *testing.T) content.Store {
	t.Helper()
	s, err := localcontent.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBuildBootIndexSelectsRequestedRootfsAndPackagesForeignKernel(t *testing.T) {
	requireMkfsErofs(t)
	ctx := context.Background()
	s := platformTestStore(t)
	x86 := platformTestComponent(t, s, KindRootfs, "linux/amd64")
	arm := platformTestComponent(t, s, KindRootfs, "linux/arm64")
	rootfsIndex, err := writeIndexToContent(ctx, s, []ocispec.Descriptor{x86, arm}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	kernel, initrd := filepath.Join(dir, "Image"), filepath.Join(dir, "initrd")
	for _, path := range []string{kernel, initrd} {
		if err := os.WriteFile(path, []byte("ARM boot asset"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target, err := BuildBootIndexInContent(ctx, s, BootIndexContentOptions{Platform: "linux/arm64", RootfsDescriptor: rootfsIndex, KernelPath: kernel, InitrdPath: initrd})
	if err != nil {
		t.Fatal(err)
	}
	info, err := InspectBootIndexContent(ctx, s, target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Platform != "linux/arm64" || info.RootfsDescriptor.Digest != arm.Digest {
		t.Fatalf("selected wrong rootfs: %#v", info)
	}
	if got, err := ComponentPlatform(ctx, s, info.SandboxDescriptor); err != nil || got != "linux/arm64" {
		t.Fatalf("kernel component platform = %q, %v", got, err)
	}
	if _, err := BuildBootIndexInContent(ctx, s, BootIndexContentOptions{Platform: "linux/arm64", RootfsDescriptor: x86, KernelPath: kernel, InitrdPath: initrd}); err == nil {
		t.Fatal("single-manifest platform mismatch accepted")
	}
}

func TestInspectBootIndexRejectsMixedPlatformsAndFalseDescriptors(t *testing.T) {
	ctx := context.Background()
	s := platformTestStore(t)
	root := platformTestComponent(t, s, KindRootfs, "linux/arm64")
	sandbox := platformTestComponent(t, s, KindSandbox, "linux/amd64")
	mixed, err := writeIndexToContent(ctx, s, []ocispec.Descriptor{root, sandbox}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBootIndexContent(ctx, s, mixed); !errors.Is(err, ErrInvalidContent) {
		t.Fatalf("mixed platforms = %v", err)
	}
	root.Platform = sandbox.Platform
	if _, err := ComponentPlatform(ctx, s, root); err == nil {
		t.Fatal("false descriptor platform accepted")
	}
}

func TestInspectLegacyBootIndexDerivesPlatformFromConfig(t *testing.T) {
	ctx := context.Background()
	s := platformTestStore(t)
	root := platformTestComponent(t, s, KindRootfs, "linux/arm64")
	sandbox := platformTestComponent(t, s, KindSandbox, "linux/arm64")
	root.Platform, sandbox.Platform = nil, nil
	target, err := writeIndexToContent(ctx, s, []ocispec.Descriptor{root, sandbox}, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := InspectBootIndexContent(ctx, s, target)
	if err != nil || info.Platform != "linux/arm64" {
		t.Fatalf("legacy platform = %#v, %v", info, err)
	}
}

func TestBootIndexPullSelectsOnePlatformAndFetchesEveryComponent(t *testing.T) {
	ctx := context.Background()
	remote, local := platformTestStore(t), platformTestStore(t)
	x86, arm := platformTestIndex(t, remote, "linux/amd64"), platformTestIndex(t, remote, "linux/arm64")
	outer, err := writeIndexToContent(ctx, remote, []ocispec.Descriptor{x86, arm}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	visited := make(map[digest.Digest]bool)
	fetch := images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		mu.Lock()
		visited[desc.Digest] = true
		mu.Unlock()
		data, err := content.ReadBlob(ctx, remote, desc)
		if err != nil {
			return nil, err
		}
		return nil, content.WriteBlob(ctx, local, desc.Digest.String(), bytes.NewReader(data), desc)
	})
	selection := bootIndexPullSelection{store: local, reference: "example/template:latest", platform: *arm.Platform}
	if err := images.Dispatch(ctx, selection.wrap(images.Handlers(fetch, images.ChildrenHandler(local))), nil, outer); err != nil {
		t.Fatal(err)
	}
	if selection.target.Digest != arm.Digest || visited[x86.Digest] {
		t.Fatalf("wrong platform fetched: target %s, x86 fetched %v", selection.target.Digest, visited[x86.Digest])
	}
	info, err := InspectBootIndexContent(ctx, local, selection.target)
	if err != nil || info.Platform != "linux/arm64" {
		t.Fatalf("incomplete selected closure: %#v, %v", info, err)
	}
	if _, err := local.Info(ctx, x86.Digest); !errdefs.IsNotFound(err) {
		t.Fatalf("unselected x86 index was downloaded: %v", err)
	}
}

func TestBootIndexPullRejectsMissingOrAmbiguousPlatform(t *testing.T) {
	ctx := context.Background()
	s := platformTestStore(t)
	x86, arm := platformTestIndex(t, s, "linux/amd64"), platformTestIndex(t, s, "linux/arm64")
	for _, children := range [][]ocispec.Descriptor{{x86}, {arm, arm}} {
		outer, err := writeIndexToContent(ctx, s, children, nil)
		if err != nil {
			t.Fatal(err)
		}
		selection := bootIndexPullSelection{store: s, reference: "example/template:latest", platform: *arm.Platform}
		if err := images.Dispatch(ctx, selection.wrap(images.ChildrenHandler(s)), nil, outer); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("invalid platform index = %v", err)
		}
	}
}
