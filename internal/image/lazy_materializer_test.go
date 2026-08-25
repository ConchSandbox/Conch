package image

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type fileFetcher struct {
	path string
}

func (f fileFetcher) Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error) {
	return os.Open(f.path)
}

type countingFetcher struct {
	fileFetcher
	fetches int
}

func (c *countingFetcher) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	c.fetches++
	return c.fileFetcher.Fetch(ctx, desc)
}

func TestLazyMemoryMaterializerBootstrapsCriticalRangesAndCommitsFullLayer(t *testing.T) {
	const (
		pageSize   = int64(4096)
		fileOffset = pageSize
		fileSize   = 8 << 20 // 8 MiB memory snapshot
		layerSize  = fileOffset + fileSize + pageSize
	)
	source := make([]byte, layerSize)
	for index := range source {
		source[index] = byte(index%251 + 1)
	}
	sourcePath := t.TempDir() + "/source.erofs"
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := LazyMemoryMetadata{
		Layer: ocispec.Descriptor{
			Digest: digest.FromBytes(source),
			Size:   layerSize,
		},
		FileOffset: fileOffset,
		FileSize:   fileSize,
	}
	stateDir := t.TempDir()
	materializer, err := newLazyMemoryMaterializer(context.Background(), fileFetcher{path: sourcePath}, stateDir, metadata)
	if err != nil {
		t.Fatal(err)
	}
	partial, err := os.ReadFile(materializer.Path())
	if err != nil {
		t.Fatal(err)
	}
	// EROFS metadata prefix is present.
	if !bytes.Equal(partial[:fileOffset], source[:fileOffset]) {
		t.Fatal("bootstrap did not fetch EROFS prefix")
	}
	// The mapped RAM region in the middle is still a hole.
	mappedStart := fileOffset + int64(64*1024)
	if !bytes.Equal(partial[mappedStart:fileOffset+fileSize-int64(1024*1024)], make([]byte, fileSize-int64(64*1024)-int64(1024*1024))) {
		t.Fatal("bootstrap fetched the mapped memory region before it was needed")
	}
	// The trailing ram-list window is present.
	if !bytes.Equal(partial[fileOffset+fileSize-int64(1024*1024):fileOffset+fileSize], source[fileOffset+fileSize-int64(1024*1024):fileOffset+fileSize]) {
		t.Fatal("bootstrap did not fetch the memory snapshot tail")
	}
	// The vmstate tail is present.
	if !bytes.Equal(partial[fileOffset+fileSize:], source[fileOffset+fileSize:]) {
		t.Fatal("bootstrap did not fetch the EROFS tail")
	}

	critical := fileOffset + int64(2<<20) // memory offset 2 MiB
	if err := materializer.MaterializeOffsets(context.Background(), pageSize, []uint64{uint64(2 << 20)}); err != nil {
		t.Fatal(err)
	}
	partial, err = os.ReadFile(materializer.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(partial[critical:critical+pageSize], source[critical:critical+pageSize]) {
		t.Fatal("critical memory page was not fetched")
	}

	if err := materializer.MaterializeAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := materializer.Commit(); err != nil {
		t.Fatal(err)
	}
	complete, err := os.ReadFile(materializer.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(complete, source) {
		t.Fatal("full materialization does not match registry layer")
	}
	if !materializer.Complete() {
		t.Fatal("verified full materialization was not committed")
	}
	store, err := localcontent.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Info(context.Background(), metadata.Layer.Digest); !errdefs.IsNotFound(err) {
		t.Fatalf("memory layer exists before on-demand commit: %v", err)
	}
	if err := commitLazyMemoryLayer(context.Background(), store, stateDir, metadata); err != nil {
		t.Fatal(err)
	}
	info, err := store.Info(context.Background(), metadata.Layer.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != metadata.Layer.Size {
		t.Fatalf("committed memory layer size = %d, want %d", info.Size, metadata.Layer.Size)
	}
}

func TestLazyMemoryMaterializerSkipsRepeatedBootstrap(t *testing.T) {
	const (
		fileOffset = int64(4096)
		fileSize   = int64(8 << 20) // 8 MiB memory snapshot
		layerSize  = fileOffset + fileSize + 4096
	)
	source := make([]byte, layerSize)
	for index := range source {
		source[index] = byte(index%251 + 1)
	}
	sourcePath := filepath.Join(t.TempDir(), "source.erofs")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := LazyMemoryMetadata{
		Layer: ocispec.Descriptor{
			Digest: digest.FromBytes(source),
			Size:   layerSize,
		},
		FileOffset: fileOffset,
		FileSize:   fileSize,
	}
	stateDir := t.TempDir()
	counting := &countingFetcher{fileFetcher: fileFetcher{path: sourcePath}}
	if _, err := newLazyMemoryMaterializer(context.Background(), counting, stateDir, metadata); err != nil {
		t.Fatal(err)
	}
	first := counting.fetches
	if first == 0 {
		t.Fatal("first bootstrap did not fetch any ranges")
	}

	// A second materializer for the same layer must not re-fetch the
	// bootstrap ranges: the EROFS prefix and memory header are already in the
	// shared file, and re-fetching them would serialize concurrent restores.
	second, err := newLazyMemoryMaterializer(context.Background(), counting, stateDir, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if counting.fetches != first {
		t.Fatalf("second bootstrap re-fetched ranges: fetches=%d want=%d", counting.fetches, first)
	}
	// Full materialization still works after the shared bootstrap.
	if err := second.MaterializeAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := second.Commit(); err != nil {
		t.Fatal(err)
	}
	complete, err := os.ReadFile(second.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(complete, source) {
		t.Fatal("materialized layer does not match registry layer")
	}
}

func TestLazyMemoryMaterializerRebootstrapsAfterFileReplacement(t *testing.T) {
	const (
		fileOffset = int64(4096)
		fileSize   = int64(8 << 20)
		layerSize  = fileOffset + fileSize + 4096
	)
	source := make([]byte, layerSize)
	for index := range source {
		source[index] = byte(index%251 + 1)
	}
	sourcePath := filepath.Join(t.TempDir(), "source.erofs")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := LazyMemoryMetadata{
		Layer: ocispec.Descriptor{
			Digest: digest.FromBytes(source),
			Size:   layerSize,
		},
		FileOffset: fileOffset,
		FileSize:   fileSize,
	}
	stateDir := t.TempDir()
	counting := &countingFetcher{fileFetcher: fileFetcher{path: sourcePath}}
	first, err := newLazyMemoryMaterializer(context.Background(), counting, stateDir, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(first.Path(), layerSize/2); err != nil {
		t.Fatal(err)
	}
	fetchesBefore := counting.fetches

	// The size-bound bootstrap marker is invalid after the file shrank, so the
	// next materializer re-fetches the bootstrap ranges instead of resuming
	// with a truncated prefix.
	if _, err := newLazyMemoryMaterializer(context.Background(), counting, stateDir, metadata); err != nil {
		t.Fatal(err)
	}
	if counting.fetches <= fetchesBefore {
		t.Fatalf("replaced file did not re-bootstrap: fetches=%d before=%d", counting.fetches, fetchesBefore)
	}
}
