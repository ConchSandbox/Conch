package image

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/transfer"
	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

func TestClassifyConchIndexKind(t *testing.T) {
	base := classifyConchIndexKind(ocispec.Index{
		Manifests: []ocispec.Descriptor{
			{Annotations: map[string]string{"io.conch.kind": "rootfs"}},
			{Annotations: map[string]string{"io.conch.kind": "sandbox"}},
		},
	})
	if base != "boot-index-cold" {
		t.Fatalf("base kind = %q, want boot-index-cold", base)
	}

	snapshot := classifyConchIndexKind(ocispec.Index{
		Manifests: []ocispec.Descriptor{
			{Annotations: map[string]string{"io.conch.kind": "rootfs"}},
			{Annotations: map[string]string{"io.conch.kind": "sandbox"}},
			{Annotations: map[string]string{"io.conch.kind": "mem-snapshot"}},
		},
	})
	if snapshot != "boot-index-resume" {
		t.Fatalf("snapshot kind = %q, want boot-index-resume", snapshot)
	}

	invalid := classifyConchIndexKind(ocispec.Index{
		Manifests: []ocispec.Descriptor{
			{Annotations: map[string]string{"io.conch.kind": "rootfs"}},
		},
	})
	if invalid != "" {
		t.Fatalf("invalid kind = %q, want empty", invalid)
	}
}

func TestNewStoresConfig(t *testing.T) {
	cfg := Config{
		DefaultKernelImage:            "registry.example.invalid/conch/kernel:6.6.0",
		DefaultKernelPlainHTTP:        true,
		DefaultKernelRegistryUsername: "kernel-user",
		DefaultKernelRegistryPassword: "kernel-pass",
	}

	svc := New(nil, cfg)

	if svc.cfg != cfg {
		t.Fatalf("config = %#v, want %#v", svc.cfg, cfg)
	}
}

func TestInferComponentKindFromName(t *testing.T) {
	cases := map[string]string{
		"localhost/conch/rootfs-component:abc":       "boot-component-rootfs",
		"localhost/conch/demo:latest-rootfs":         "boot-component-rootfs",
		"localhost/conch/sandbox-component:def":      "boot-component-sandbox",
		"localhost/conch/demo:latest-sandbox":        "boot-component-sandbox",
		"localhost/conch/mem-snapshot-component:ghi": "boot-component-memory",
		"localhost/conch/demo:latest-mem":            "boot-component-memory",
		"localhost/conch/demo:latest":                "",
	}
	for name, want := range cases {
		if got := inferComponentKindFromName(name); got != want {
			t.Fatalf("%s => %q, want %q", name, got, want)
		}
	}
}

func TestExternalComponentKindMapsContentAnnotations(t *testing.T) {
	cases := map[string]string{
		"rootfs":                 "boot-component-rootfs",
		"sandbox":                "boot-component-sandbox",
		"mem-snapshot":           "boot-component-memory",
		"boot-index-cold":        "",
		"boot-index-resume":      "",
		"boot-component-rootfs":  "",
		"boot-component-sandbox": "",
		"boot-component-memory":  "",
		"oci-image":              "",
		"sandbox-base":           "",
		"sandbox-snapshot":       "",
		"custom":                 "",
	}
	for input, want := range cases {
		if got := externalComponentKind(input); got != want {
			t.Fatalf("externalComponentKind(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestResolveRegistryResponseHeaderTimeout(t *testing.T) {
	t.Setenv(registryTimeoutEnv, "10m")
	if got := resolveRegistryResponseHeaderTimeout(""); got != 10*time.Minute {
		t.Fatalf("timeout = %s, want 10m", got)
	}

	t.Setenv(registryTimeoutEnv, "bad")
	if got := resolveRegistryResponseHeaderTimeout(""); got != defaultRegistryResponseHeaderTimeout {
		t.Fatalf("timeout = %s, want default %s", got, defaultRegistryResponseHeaderTimeout)
	}

	t.Setenv(registryTimeoutEnv, "0")
	if got := resolveRegistryResponseHeaderTimeout(""); got != defaultRegistryResponseHeaderTimeout {
		t.Fatalf("timeout = %s, want default %s", got, defaultRegistryResponseHeaderTimeout)
	}

	t.Setenv(registryTimeoutEnv, "10m")
	if got := resolveRegistryResponseHeaderTimeout("3m"); got != 3*time.Minute {
		t.Fatalf("request registry timeout = %s, want 3m", got)
	}
}

func TestRegistryHTTPClientUsesResolvedResponseHeaderTimeout(t *testing.T) {
	t.Setenv(registryTimeoutEnv, "7m")

	client := registryHTTPClient("")
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.ResponseHeaderTimeout != 7*time.Minute {
		t.Fatalf("ResponseHeaderTimeout = %s, want 7m", transport.ResponseHeaderTimeout)
	}
}

func TestComponentProgressDescriptorsFromRemoteTargetMapsConchKinds(t *testing.T) {
	rootfsManifest, rootfsRaw := testJSONDescriptor(t, ocispec.MediaTypeImageManifest, ocispec.Manifest{
		Config: ocispec.Descriptor{Digest: digest.FromString("rootfs-config"), Size: 10},
		Layers: []ocispec.Descriptor{{Digest: digest.FromString("rootfs-layer"), Size: 100}},
	})
	sandboxManifest, sandboxRaw := testJSONDescriptor(t, ocispec.MediaTypeImageManifest, ocispec.Manifest{
		Config: ocispec.Descriptor{Digest: digest.FromString("sandbox-config"), Size: 20},
		Layers: []ocispec.Descriptor{{Digest: digest.FromString("sandbox-layer"), Size: 200}},
	})
	memManifest, memRaw := testJSONDescriptor(t, ocispec.MediaTypeImageManifest, ocispec.Manifest{
		Config: ocispec.Descriptor{Digest: digest.FromString("mem-config"), Size: 30},
		Layers: []ocispec.Descriptor{{Digest: digest.FromString("mem-layer"), Size: 300}},
	})
	rootfsManifest.Annotations = map[string]string{"io.conch.kind": "rootfs"}
	sandboxManifest.Annotations = map[string]string{"io.conch.kind": "sandbox"}
	memManifest.Annotations = map[string]string{"io.conch.kind": "mem-snapshot"}
	indexDesc, indexRaw := testJSONDescriptor(t, ocispec.MediaTypeImageIndex, ocispec.Index{
		Manifests: []ocispec.Descriptor{rootfsManifest, sandboxManifest, memManifest, rootfsManifest},
	})
	fetcher := testRemoteFetcher{
		indexDesc.Digest.String():       indexRaw,
		rootfsManifest.Digest.String():  rootfsRaw,
		sandboxManifest.Digest.String(): sandboxRaw,
		memManifest.Digest.String():     memRaw,
	}

	descs := componentProgressDescriptorsFromRemoteTarget(context.Background(), fetcher, indexDesc, "")

	components := map[string]bool{}
	digests := map[string]bool{}
	for _, desc := range descs {
		components[desc.component] = true
		if digests[desc.digest] {
			t.Fatalf("duplicate progress descriptor digest %q in %#v", desc.digest, descs)
		}
		digests[desc.digest] = true
	}
	for _, want := range []string{"rootfs", "kernel", "mem-snapshot"} {
		if !components[want] {
			t.Fatalf("components = %#v, missing %q", components, want)
		}
	}
	if components["overall"] {
		t.Fatalf("classified Conch index should not include overall component: %#v", components)
	}
}

func TestComponentProgressDescriptorsFromRemoteTargetFallbackUsesOverrideComponent(t *testing.T) {
	target := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    digest.FromString("kernel-index"),
		Size:      123,
	}

	descs := componentProgressDescriptorsFromRemoteTarget(context.Background(), testRemoteFetcher{}, target, "kernel")

	if len(descs) != 1 || descs[0].component != "kernel" || descs[0].size != 123 {
		t.Fatalf("fallback descriptors = %#v, want kernel descriptor", descs)
	}
}

func TestPullProgressFetcherReportsBytesRead(t *testing.T) {
	desc := ocispec.Descriptor{
		Digest: digest.FromString("rootfs-layer"),
		Size:   10,
	}
	tracker, latest := newTestPullProgressTracker(t, pullProgressDescriptor{
		component: "rootfs",
		digest:    desc.Digest.String(),
		size:      desc.Size,
	})
	fetcher := newPullProgressFetcher(testRemoteFetcher{
		desc.Digest.String(): []byte("0123456789"),
	}, tracker, "rootfs")

	rc, err := fetcher.Fetch(context.Background(), desc)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer rc.Close()
	if _, err := io.ReadFull(rc, make([]byte, 4)); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}

	if got := latest()["rootfs"].Progress; got != 4 {
		t.Fatalf("rootfs progress = %d, want 4 bytes read", got)
	}
}

func TestPullProgressFetcherPreservesSeekAndDeduplicatesReaders(t *testing.T) {
	desc := ocispec.Descriptor{
		Digest: digest.FromString("resumable-layer"),
		Size:   10,
	}
	tracker, latest := newTestPullProgressTracker(t, pullProgressDescriptor{
		component: "rootfs",
		digest:    desc.Digest.String(),
		size:      desc.Size,
	})
	fetcher := newPullProgressFetcher(testSeekableFetcher{data: []byte("0123456789")}, tracker, "rootfs")

	first, err := fetcher.Fetch(context.Background(), desc)
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}
	seeker, ok := first.(io.Seeker)
	if !ok {
		t.Fatalf("wrapped reader type %T does not preserve io.Seeker", first)
	}
	if offset, err := seeker.Seek(4, io.SeekStart); err != nil || offset != 4 {
		t.Fatalf("Seek() = (%d, %v), want (4, nil)", offset, err)
	}
	if _, err := io.ReadFull(first, make([]byte, 2)); err != nil {
		t.Fatalf("first ReadFull: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := fetcher.Fetch(context.Background(), desc)
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	defer second.Close()
	if _, err := io.ReadFull(second, make([]byte, 3)); err != nil {
		t.Fatalf("second initial ReadFull: %v", err)
	}
	tracker.Emit()
	if got := latest()["rootfs"].Progress; got != 6 {
		t.Fatalf("progress after shorter retry = %d, want previous maximum 6", got)
	}

	if _, err := io.ReadFull(second, make([]byte, 5)); err != nil {
		t.Fatalf("second resumed ReadFull: %v", err)
	}
	tracker.Emit()
	if got := latest()["rootfs"].Progress; got != 8 {
		t.Fatalf("progress after retry advances = %d, want 8", got)
	}
}

func TestPullProgressResolverForwardsResolverOptions(t *testing.T) {
	base := &testResolverWithOptions{}
	wrapped := newPullProgressResolver(newPullPinnedResolver(base), nil, "")
	resolverWithOptions, ok := wrapped.(remotes.ResolverWithOptions)
	if !ok {
		t.Fatalf("wrapped resolver type %T does not preserve ResolverWithOptions", wrapped)
	}

	resolverWithOptions.SetOptions()

	if base.setOptionsCalls != 1 {
		t.Fatalf("SetOptions calls = %d, want 1", base.setOptionsCalls)
	}
}

func TestPullPinnedResolverPinsResolvedDescriptor(t *testing.T) {
	first := ocispec.Descriptor{Digest: digest.FromString("first-target"), Size: 10}
	second := ocispec.Descriptor{Digest: digest.FromString("second-target"), Size: 20}
	base := &changingTestResolver{targets: []ocispec.Descriptor{first, second}}
	wrapped := newPullPinnedResolver(base)

	firstName, firstTarget, err := wrapped.Resolve(context.Background(), "registry.example/demo:latest")
	if err != nil {
		t.Fatalf("first Resolve() error = %v", err)
	}
	secondName, secondTarget, err := wrapped.Resolve(context.Background(), "registry.example/demo:latest")
	if err != nil {
		t.Fatalf("second Resolve() error = %v", err)
	}

	if firstName != secondName || firstTarget.Digest != secondTarget.Digest || firstTarget.Size != secondTarget.Size {
		t.Fatalf("Resolve() changed within one pull: first=(%q, %#v), second=(%q, %#v)", firstName, firstTarget, secondName, secondTarget)
	}
	if base.resolveCalls != 1 {
		t.Fatalf("underlying Resolve() calls = %d, want 1", base.resolveCalls)
	}
}

func TestPullProgressTrackerSuppressesUnchangedEvents(t *testing.T) {
	desc := pullProgressDescriptor{
		component: "rootfs",
		digest:    digest.FromString("unchanged-layer").String(),
		size:      10,
	}
	tracker, eventCount := newCountingPullProgressTracker(t, desc)

	tracker.Emit()
	tracker.Emit()

	if got := eventCount(); got != 1 {
		t.Fatalf("unchanged progress events = %d, want 1", got)
	}
}

func TestPullProgressTrackerMarksCommittedContentComplete(t *testing.T) {
	ctx := context.Background()
	store, err := localcontent.NewStore(filepath.Join(t.TempDir(), "content"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	payload := []byte("already downloaded")
	desc := ocispec.Descriptor{
		Digest: digest.FromBytes(payload),
		Size:   int64(len(payload)),
	}
	if err := content.WriteBlob(ctx, store, "cached-layer", bytes.NewReader(payload), desc); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	var latest runtimeapi.PullProgress
	tracker := newPullProgressTracker(ctx, store, func(progress runtimeapi.PullProgress) {
		latest = progress
	})
	tracker.SetDescriptors([]pullProgressDescriptor{{
		component: "rootfs",
		digest:    desc.Digest.String(),
		size:      desc.Size,
	}})

	tracker.Emit()

	if latest.Progress != desc.Size || latest.Total != desc.Size {
		t.Fatalf("cached progress = %d/%d, want %d/%d", latest.Progress, latest.Total, desc.Size, desc.Size)
	}
}

func TestPullProgressFetcherSupportsConcurrentReaders(t *testing.T) {
	desc := ocispec.Descriptor{
		Digest: digest.FromString("concurrent-layer"),
		Size:   64,
	}
	tracker, latest := newTestPullProgressTracker(t, pullProgressDescriptor{
		component: "rootfs",
		digest:    desc.Digest.String(),
		size:      desc.Size,
	})
	fetcher := newPullProgressFetcher(testSeekableFetcher{data: bytes.Repeat([]byte{'x'}, int(desc.Size))}, tracker, "rootfs")

	var wg sync.WaitGroup
	for i := 1; i <= 8; i++ {
		readSize := i * 8
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc, err := fetcher.Fetch(context.Background(), desc)
			if err != nil {
				t.Errorf("Fetch: %v", err)
				return
			}
			defer rc.Close()
			if _, err := io.ReadFull(rc, make([]byte, readSize)); err != nil {
				t.Errorf("ReadFull(%d): %v", readSize, err)
			}
		}()
	}
	wg.Wait()
	tracker.Emit()

	if got := latest()["rootfs"].Progress; got != desc.Size {
		t.Fatalf("concurrent progress = %d, want %d", got, desc.Size)
	}
}

func TestSelectImportedSnapshotFallsBackForRegularOCIIndex(t *testing.T) {
	imported := []images.Image{{
		Name:   "example:latest",
		Target: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex},
	}}
	var regularUnpackCalls int

	snapshotKey, imageName, err := selectImportedSnapshot(imported,
		func(images.Image) (map[string]string, bool, error) {
			return nil, false, nil
		},
		func(imgInfo images.Image) (string, error) {
			regularUnpackCalls++
			if imgInfo.Name != "example:latest" {
				t.Fatalf("regular unpack image = %q, want example:latest", imgInfo.Name)
			}
			return "regular-snapshot", nil
		},
	)
	if err != nil {
		t.Fatalf("selectImportedSnapshot() error = %v", err)
	}
	if snapshotKey != "regular-snapshot" || imageName != "example:latest" {
		t.Fatalf("selectImportedSnapshot() = (%q, %q), want (regular-snapshot, example:latest)", snapshotKey, imageName)
	}
	if regularUnpackCalls != 1 {
		t.Fatalf("regular unpack calls = %d, want 1", regularUnpackCalls)
	}
}

func TestSelectImportedSnapshotReturnsConchIndexUnpackError(t *testing.T) {
	imported := []images.Image{{
		Name:   "conch:latest",
		Target: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex},
	}}
	unpackErr := errors.New("unpack conch index")

	_, _, err := selectImportedSnapshot(imported,
		func(images.Image) (map[string]string, bool, error) {
			return nil, true, unpackErr
		},
		func(images.Image) (string, error) {
			t.Fatal("regular unpack should not be called for a valid Conch index")
			return "", nil
		},
	)
	if !errors.Is(err, unpackErr) {
		t.Fatalf("selectImportedSnapshot() error = %v, want %v", err, unpackErr)
	}
}

type testRemoteFetcher map[string][]byte

func (f testRemoteFetcher) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	raw, ok := f[desc.Digest.String()]
	if !ok {
		return nil, errors.New("missing test descriptor")
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

type testSeekableFetcher struct {
	data []byte
}

func (f testSeekableFetcher) Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error) {
	return &testReadSeekCloser{Reader: bytes.NewReader(f.data)}, nil
}

type testReadSeekCloser struct {
	*bytes.Reader
}

func (*testReadSeekCloser) Close() error { return nil }

type testResolverWithOptions struct {
	setOptionsCalls int
}

type changingTestResolver struct {
	targets      []ocispec.Descriptor
	resolveCalls int
}

func (r *changingTestResolver) Resolve(_ context.Context, ref string) (string, ocispec.Descriptor, error) {
	if len(r.targets) == 0 {
		return "", ocispec.Descriptor{}, errors.New("no targets configured")
	}
	index := r.resolveCalls
	if index >= len(r.targets) {
		index = len(r.targets) - 1
	}
	r.resolveCalls++
	return ref, r.targets[index], nil
}

func (*changingTestResolver) Fetcher(context.Context, string) (remotes.Fetcher, error) {
	return testRemoteFetcher{}, nil
}

func (*changingTestResolver) Pusher(context.Context, string) (remotes.Pusher, error) {
	return nil, errors.New("not implemented")
}

func (*testResolverWithOptions) Resolve(context.Context, string) (string, ocispec.Descriptor, error) {
	return "", ocispec.Descriptor{}, nil
}

func (*testResolverWithOptions) Fetcher(context.Context, string) (remotes.Fetcher, error) {
	return testRemoteFetcher{}, nil
}

func (*testResolverWithOptions) Pusher(context.Context, string) (remotes.Pusher, error) {
	return nil, errors.New("not implemented")
}

func (r *testResolverWithOptions) SetOptions(...transfer.ImageResolverOption) {
	r.setOptionsCalls++
}

func newTestPullProgressTracker(t *testing.T, descs ...pullProgressDescriptor) (*pullProgressTracker, func() map[string]runtimeapi.PullProgress) {
	t.Helper()
	store, err := localcontent.NewStore(filepath.Join(t.TempDir(), "content"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	latest := make(map[string]runtimeapi.PullProgress)
	tracker := newPullProgressTracker(context.Background(), store, func(progress runtimeapi.PullProgress) {
		latest[progress.Component] = progress
	})
	tracker.SetDescriptors(descs)
	return tracker, func() map[string]runtimeapi.PullProgress { return latest }
}

func newCountingPullProgressTracker(t *testing.T, descs ...pullProgressDescriptor) (*pullProgressTracker, func() int) {
	t.Helper()
	store, err := localcontent.NewStore(filepath.Join(t.TempDir(), "content"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	count := 0
	tracker := newPullProgressTracker(context.Background(), store, func(runtimeapi.PullProgress) {
		count++
	})
	tracker.SetDescriptors(descs)
	return tracker, func() int { return count }
}

func testJSONDescriptor(t *testing.T, mediaType string, value any) (ocispec.Descriptor, []byte) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal test descriptor: %v", err)
	}
	return ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.FromBytes(raw),
		Size:      int64(len(raw)),
	}, raw
}
