package image

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestFetchDescriptorsRunsConcurrentlyAndDeduplicates(t *testing.T) {
	descriptors := []ocispec.Descriptor{
		{Digest: digest.FromString("a")},
		{Digest: digest.FromString("b")},
		{Digest: digest.FromString("a")},
		{Digest: digest.FromString("c")},
	}
	var calls atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32
	allStarted := make(chan struct{})
	release := make(chan struct{})
	var closeOnce sync.Once
	done := make(chan error, 1)
	go func() {
		done <- fetchDescriptors(context.Background(), descriptors, func(_ context.Context, _ ocispec.Descriptor) error {
			current := active.Add(1)
			for {
				maximum := maxActive.Load()
				if current <= maximum || maxActive.CompareAndSwap(maximum, current) {
					break
				}
			}
			if calls.Add(1) == 3 {
				closeOnce.Do(func() { close(allStarted) })
			}
			<-release
			active.Add(-1)
			return nil
		})
	}()

	select {
	case <-allStarted:
		close(release)
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("descriptor fetches did not run concurrently")
	}
	if err := <-done; err != nil {
		t.Fatalf("fetchDescriptors() error = %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("fetch calls = %d, want 3 unique descriptors", got)
	}
	if got := maxActive.Load(); got != 3 {
		t.Fatalf("maximum concurrent fetches = %d, want 3", got)
	}
}

func TestFetchDescriptorsReturnsFetchError(t *testing.T) {
	want := errors.New("fetch failed")
	err := fetchDescriptors(context.Background(), []ocispec.Descriptor{{Digest: digest.FromString("a")}}, func(context.Context, ocispec.Descriptor) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("fetchDescriptors() error = %v, want %v", err, want)
	}
}

func TestParseLazyMemoryMetadataAllowsMissingProfile(t *testing.T) {
	manifest := ocispec.Manifest{
		Layers: []ocispec.Descriptor{{
			Size: 8192,
			Annotations: map[string]string{
				AnnotationMemoryFileOffset: "4096",
				AnnotationMemoryFileSize:   "2048",
			},
		}},
	}
	metadata, err := parseLazyMemoryMetadata("", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.FileOffset != 4096 || metadata.FileSize != 2048 {
		t.Fatalf("parsed metadata = %+v", metadata)
	}
	if len(metadata.Profile) != 0 {
		t.Fatalf("missing profile decoded into %d bytes", len(metadata.Profile))
	}
}

func TestParseLazyMemoryMetadataRejectsInvalidExtent(t *testing.T) {
	manifest := ocispec.Manifest{
		Layers: []ocispec.Descriptor{{
			Size: 4096,
			Annotations: map[string]string{
				AnnotationMemoryFileOffset: "4096",
				AnnotationMemoryFileSize:   "4097",
			},
		}},
	}
	if _, err := parseLazyMemoryMetadata("", manifest); err == nil {
		t.Fatal("memory file size beyond the EROFS layer was accepted")
	}
}
