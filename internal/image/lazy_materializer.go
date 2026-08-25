package image

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/openeuler/Conch/pkg/ulog"
)

type LazyMemoryMaterializer struct {
	metadata        LazyMemoryMetadata
	fetcher         remotes.Fetcher
	path            string
	marker          string
	bootstrapMarker string
	state           *lazyMemoryState
}

type byteRange struct {
	start uint64
	end   uint64
}

type lazyMemoryState struct {
	sync.Mutex
	criticalRanges []byteRange
}

var lazyMemoryStates sync.Map

func NewLazyMemoryMaterializer(
	ctx context.Context,
	reference string,
	plainHTTP bool,
	stateDir string,
	metadata LazyMemoryMetadata,
) (*LazyMemoryMaterializer, error) {
	if strings.TrimSpace(reference) == "" {
		return nil, fmt.Errorf("lazy memory registry reference is required")
	}
	if strings.TrimSpace(stateDir) == "" {
		return nil, fmt.Errorf("lazy memory state directory is required")
	}
	resolver := docker.NewResolver(docker.ResolverOptions{PlainHTTP: plainHTTP})
	name, _, err := resolver.Resolve(ctx, reference)
	if err != nil {
		return nil, fmt.Errorf("resolve lazy memory source: %w", err)
	}
	fetcher, err := resolver.Fetcher(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("create lazy memory fetcher: %w", err)
	}
	return newLazyMemoryMaterializer(ctx, fetcher, stateDir, metadata)
}

func newLazyMemoryMaterializer(ctx context.Context, fetcher remotes.Fetcher, stateDir string, metadata LazyMemoryMetadata) (*LazyMemoryMaterializer, error) {
	dir := filepath.Join(stateDir, "lazy-memory")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path, marker, bootstrapMarker := lazyMemoryPaths(stateDir, metadata.Layer)
	stateValue, _ := lazyMemoryStates.LoadOrStore(path, &lazyMemoryState{})
	materializer := &LazyMemoryMaterializer{
		metadata:        metadata,
		fetcher:         fetcher,
		path:            path,
		marker:          marker,
		bootstrapMarker: bootstrapMarker,
		state:           stateValue.(*lazyMemoryState),
	}
	if err := materializer.prepareBootstrap(ctx); err != nil {
		return nil, err
	}
	return materializer, nil
}

func lazyMemoryPaths(stateDir string, layer ocispec.Descriptor) (path, marker, bootstrapMarker string) {
	namePart := layer.Digest.Algorithm().String() + "-" + layer.Digest.Encoded()
	dir := filepath.Join(stateDir, "lazy-memory")
	return filepath.Join(dir, namePart+".erofs"),
		filepath.Join(dir, namePart+".complete"),
		filepath.Join(dir, namePart+".bootstrap")
}

func commitLazyMemoryLayer(ctx context.Context, store content.Store, stateDir string, metadata LazyMemoryMetadata) error {
	if store == nil {
		return fmt.Errorf("content store is required")
	}
	if _, err := store.Info(ctx, metadata.Layer.Digest); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	path, marker, _ := lazyMemoryPaths(stateDir, metadata.Layer)
	markerData, err := os.ReadFile(marker)
	if err != nil {
		return fmt.Errorf("lazy memory layer is not fully materialized: %w", err)
	}
	if strings.TrimSpace(string(markerData)) != metadata.Layer.Digest.String() {
		return fmt.Errorf("lazy memory completion marker does not match %s", metadata.Layer.Digest)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open materialized lazy memory layer: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat materialized lazy memory layer: %w", err)
	}
	if info.Size() != metadata.Layer.Size {
		return fmt.Errorf("materialized lazy memory layer size %d, want %d", info.Size(), metadata.Layer.Size)
	}
	if err := content.WriteBlob(ctx, store, contentRef("lazy-memory", metadata.Layer.Digest), file, metadata.Layer); err != nil {
		return fmt.Errorf("commit lazy memory layer %s to content store: %w", metadata.Layer.Digest, err)
	}
	return nil
}

func (m *LazyMemoryMaterializer) Path() string {
	if m == nil {
		return ""
	}
	return m.path
}

func (m *LazyMemoryMaterializer) Complete() bool {
	if m == nil {
		return false
	}
	marker, err := os.ReadFile(m.marker)
	if err != nil || strings.TrimSpace(string(marker)) != m.metadata.Layer.Digest.String() {
		return false
	}
	info, err := os.Stat(m.path)
	return err == nil && info.Size() == m.metadata.Layer.Size
}

func (m *LazyMemoryMaterializer) Profile() []byte {
	if m == nil {
		return nil
	}
	return append([]byte(nil), m.metadata.Profile...)
}

func (m *LazyMemoryMaterializer) prepareBootstrap(ctx context.Context) error {
	// Fast path: another materializer for the same layer already fetched the
	// EROFS prefix and memory header. Without this, every concurrent restore of
	// the same template re-fetches them in turn and holds the per-layer lock,
	// delaying the full materialization that the resume gates depend on.
	if m.bootstrapDone() || m.Complete() {
		return nil
	}
	m.state.Lock()
	defer m.state.Unlock()
	if m.bootstrapDone() || m.Complete() {
		return nil
	}
	m.state.criticalRanges = nil
	file, err := os.OpenFile(m.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != m.metadata.Layer.Size {
		if err := file.Truncate(m.metadata.Layer.Size); err != nil {
			return err
		}
	}
	// EROFS metadata prefix (superblock, inode table, directory blocks) plus
	// the memory snapshot file's leading header (migration header and ram-list
	// descriptor, which restore reads before the mapped RAM region). The
	// profile only covers mapped pages, so the header is never in it: fetch up
	// to the first mapped offset (or a conservative default when the profile
	// is not known yet). Merged into one range request.
	headerBytes := int64(64 * 1024)
	if len(m.metadata.Profile) != 0 {
		var identity struct {
			Offsets []uint64 `json:"offsets"`
		}
		if decodeErr := json.Unmarshal(m.metadata.Profile, &identity); decodeErr == nil && len(identity.Offsets) != 0 {
			headerBytes = int64(identity.Offsets[0])
		}
	}
	if headerBytes > m.metadata.FileSize {
		headerBytes = m.metadata.FileSize
	}
	if err := m.copyRange(ctx, file, 0, m.metadata.FileOffset+headerBytes); err != nil {
		return fmt.Errorf("fetch lazy EROFS prefix and memory header: %w", err)
	}
	// The ram-list section is written after the mapped RAM region and is also
	// read during restore before the gate opens. It is outside the mapped
	// region, so prefetch a conservative trailing window of the memory file
	// together with the trailing EROFS metadata and the vmstate file.
	tailBytes := int64(1024 * 1024)
	if tailBytes > m.metadata.FileSize {
		tailBytes = m.metadata.FileSize
	}
	tailStart := m.metadata.FileOffset + m.metadata.FileSize - tailBytes
	if err := m.copyRange(ctx, file, tailStart, m.metadata.Layer.Size-tailStart); err != nil {
		return fmt.Errorf("fetch lazy memory tail and EROFS tail: %w", err)
	}
	if err := file.Sync(); err != nil {
		return err
	}
	// Commit only after both ranges landed, so a partial fetch is retried.
	return m.commitBootstrap()
}

func (m *LazyMemoryMaterializer) bootstrapDone() bool {
	data, err := os.ReadFile(m.bootstrapMarker)
	if err != nil {
		return false
	}
	if strings.TrimSpace(string(data)) != strconv.FormatInt(m.metadata.Layer.Size, 10) {
		return false
	}
	info, err := os.Stat(m.path)
	return err == nil && info.Size() == m.metadata.Layer.Size
}

func (m *LazyMemoryMaterializer) commitBootstrap() error {
	data := []byte(strconv.FormatInt(m.metadata.Layer.Size, 10) + "\n")
	temp := m.bootstrapMarker + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, m.bootstrapMarker)
}

func (m *LazyMemoryMaterializer) MaterializeOffsets(ctx context.Context, pageSize int64, offsets []uint64) error {
	if m == nil || m.Complete() || len(offsets) == 0 {
		return nil
	}
	if pageSize <= 0 {
		return fmt.Errorf("invalid pre-gate page size %d", pageSize)
	}
	requested := normalizeByteRanges(uint64(pageSize), uint64(m.metadata.FileSize), offsets)
	if len(requested) == 0 {
		return nil
	}
	m.state.Lock()
	defer m.state.Unlock()
	if m.Complete() {
		return nil
	}
	missing := subtractByteRanges(requested, m.state.criticalRanges)
	if len(missing) == 0 {
		return nil
	}
	file, err := os.OpenFile(m.path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, current := range missing {
		physical := m.metadata.FileOffset + int64(current.start)
		if err := m.copyRange(ctx, file, physical, int64(current.end-current.start)); err != nil {
			return fmt.Errorf("fetch restore-critical memory range at %d: %w", current.start, err)
		}
	}
	if err := file.Sync(); err != nil {
		return err
	}
	m.state.criticalRanges = mergeByteRanges(append(m.state.criticalRanges, missing...))
	return nil
}

func (m *LazyMemoryMaterializer) MaterializeAll(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("lazy memory materializer is nil")
	}
	m.state.Lock()
	defer m.state.Unlock()
	if m.Complete() {
		return nil
	}
	start := time.Now()
	remote, err := m.fetcher.Fetch(ctx, m.metadata.Layer)
	if err != nil {
		return err
	}
	defer remote.Close()
	file, err := os.OpenFile(m.path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	verifier := m.metadata.Layer.Digest.Verifier()
	buffer := make([]byte, 4<<20)
	var offset int64
	for offset < m.metadata.Layer.Size {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := int64(len(buffer))
		if remaining := m.metadata.Layer.Size - offset; remaining < want {
			want = remaining
		}
		n, readErr := io.ReadFull(remote, buffer[:want])
		if n > 0 {
			if _, err := verifier.Write(buffer[:n]); err != nil {
				return err
			}
			if _, err := file.WriteAt(buffer[:n], offset); err != nil {
				return err
			}
			offset += int64(n)
		}
		if readErr != nil {
			return fmt.Errorf("read lazy memory layer at %d: %w", offset, readErr)
		}
	}
	if !verifier.Verified() {
		return fmt.Errorf("lazy memory layer digest verification failed")
	}
	ulog.GetLogger().Info("Lazy memory layer materialized", ulog.F("digest", m.metadata.Layer.Digest), ulog.F("bytes", m.metadata.Layer.Size), ulog.F("elapsed", time.Since(start)))
	// Mark complete before returning: the data is verified in the page cache,
	// so concurrent materializers skip the full fetch. The fsync is deferred to
	// Commit(), which runs after the resume gate is signalled.
	tempMarker := m.marker + ".tmp"
	if err := os.WriteFile(tempMarker, []byte(m.metadata.Layer.Digest.String()+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tempMarker, m.marker)
}

func normalizeByteRanges(pageSize, fileSize uint64, offsets []uint64) []byteRange {
	ranges := make([]byteRange, 0, len(offsets))
	for _, start := range offsets {
		if start >= fileSize {
			continue
		}
		end := start + pageSize
		if end < start || end > fileSize {
			end = fileSize
		}
		ranges = append(ranges, byteRange{start: start, end: end})
	}
	return mergeByteRanges(ranges)
}

func mergeByteRanges(ranges []byteRange) []byteRange {
	if len(ranges) == 0 {
		return nil
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	merged := ranges[:1]
	for _, current := range ranges[1:] {
		last := &merged[len(merged)-1]
		if current.start <= last.end {
			if current.end > last.end {
				last.end = current.end
			}
			continue
		}
		merged = append(merged, current)
	}
	return merged
}

func subtractByteRanges(requested, covered []byteRange) []byteRange {
	missing := append([]byteRange(nil), requested...)
	for _, existing := range covered {
		next := make([]byteRange, 0, len(missing)+1)
		for _, current := range missing {
			if existing.end <= current.start || existing.start >= current.end {
				next = append(next, current)
				continue
			}
			if existing.start > current.start {
				next = append(next, byteRange{start: current.start, end: existing.start})
			}
			if existing.end < current.end {
				next = append(next, byteRange{start: existing.end, end: current.end})
			}
		}
		missing = next
	}
	return missing
}

// Commit persists the materialized layer to stable storage. It runs after the
// resume gate has already been signalled: the gate only requires the verified
// data to be in the page cache, so the fsync (a few hundred ms on a large
// layer) is not on the restore critical path.
func (m *LazyMemoryMaterializer) Commit() error {
	if m == nil {
		return nil
	}
	file, err := os.OpenFile(m.path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func (m *LazyMemoryMaterializer) copyRange(ctx context.Context, file *os.File, offset, length int64) error {
	if length <= 0 {
		return nil
	}
	remote, err := m.fetcher.Fetch(ctx, m.metadata.Layer)
	if err != nil {
		return err
	}
	defer remote.Close()
	seeker, ok := remote.(io.Seeker)
	if !ok {
		return fmt.Errorf("registry fetcher does not support range seeks")
	}
	if _, err := seeker.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	writer := io.NewOffsetWriter(file, offset)
	written, err := io.CopyN(writer, remote, length)
	if err != nil {
		return err
	}
	if written != length {
		return io.ErrUnexpectedEOF
	}
	return nil
}
