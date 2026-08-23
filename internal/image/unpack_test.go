package image

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestSerializeUnpack(t *testing.T) {
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	released := false
	release := func() {
		if !released {
			close(releaseFirst)
			released = true
		}
	}
	defer release()

	go func() {
		firstDone <- serializeUnpack(context.Background(), "erofs", "sha256:chain", func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	secondCalled := false
	go func() {
		secondDone <- serializeUnpack(secondCtx, "erofs", "sha256:chain", func() error {
			secondCalled = true
			return nil
		})
	}()
	waitForUnpackLockRefs(t, "sn://erofs/sha256:chain", 2)
	cancelSecond()
	select {
	case err := <-secondDone:
		if err != context.Canceled {
			t.Fatalf("second serializeUnpack() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		release()
		<-firstDone
		t.Fatal("canceled duplicate unpack remained blocked")
	}
	if secondCalled {
		t.Fatal("duplicate unpack entered before the first unpack completed")
	}

	for _, tc := range []struct {
		name        string
		snapshotter string
		chainID     string
	}{
		{name: "chain ID", snapshotter: "erofs", chainID: "sha256:other"},
		{name: "snapshotter", snapshotter: "overlayfs", chainID: "sha256:chain"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := serializeUnpack(ctx, tc.snapshotter, tc.chainID, func() error { return nil })
		cancel()
		if err != nil {
			t.Fatalf("unpack with independent %s: %v", tc.name, err)
		}
	}
	release()
	if err := <-firstDone; err != nil {
		t.Fatalf("first serializeUnpack() error = %v", err)
	}
}

func waitForUnpackLockRefs(t *testing.T, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		componentUnpackLocks.mu.Lock()
		entry := componentUnpackLocks.locks[key]
		got := 0
		if entry != nil {
			got = entry.refs
		}
		componentUnpackLocks.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("unpack lock %q did not reach %d references", key, want)
}

func TestGetKindDefaultsToUnknown(t *testing.T) {
	got := getKind(ocispec.Descriptor{})
	if got != KindUnknown {
		t.Fatalf("kind: got %q want %q", got, KindUnknown)
	}
}

func TestValidateNativeComponentManifestRejectsEmptyLayers(t *testing.T) {
	ctx := context.Background()
	store, err := localcontent.NewStore(filepath.Join(t.TempDir(), "content"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	client, err := containerd.New("", containerd.WithServices(containerd.WithContentStore(store)))
	if err != nil {
		t.Fatalf("new containerd client: %v", err)
	}

	manifestDesc, err := writeBlobJSONToContent(ctx, store, ocispec.Manifest{
		Versioned: ispec.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    digest.FromString("empty-component-config"),
			Size:      1,
		},
	}, ocispec.MediaTypeImageManifest)
	if err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	for _, kind := range []string{KindRootfs, KindSandbox, KindMemSnapshot} {
		t.Run(kind, func(t *testing.T) {
			err := validateNativeComponentManifest(ctx, client, kind, manifestDesc)
			if err == nil || !strings.Contains(err.Error(), "no layers") {
				t.Fatalf("validateNativeComponentManifest() error = %v, want no layers", err)
			}
		})
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
