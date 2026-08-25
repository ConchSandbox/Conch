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

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"

	"github.com/openeuler/Conch/pkg/ulog"
)

type LazyMemoryMaterializer struct {
	metadata        LazyMemoryMetadata
	fetcher         remotes.Fetcher
	path            string
	marker          string
	bootstrapMarker string
	lock            *sync.Mutex
}

var lazyMemoryLocks sync.Map

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
	namePart := metadata.Layer.Digest.Algorithm().String() + "-" + metadata.Layer.Digest.Encoded()
	lockValue, _ := lazyMemoryLocks.LoadOrStore(namePart, &sync.Mutex{})
	materializer := &LazyMemoryMaterializer{
		metadata:        metadata,
		fetcher:         fetcher,
		path:            filepath.Join(dir, namePart+".erofs"),
		marker:          filepath.Join(dir, namePart+".complete"),
		bootstrapMarker: filepath.Join(dir, namePart+".bootstrap"),
		lock:            lockValue.(*sync.Mutex),
	}
	if err := materializer.prepareBootstrap(ctx); err != nil {
		return nil, err
	}
	return materializer, nil
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
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.bootstrapDone() || m.Complete() {
		return nil
	}
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
	ordered := append([]uint64(nil), offsets...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	file, err := os.OpenFile(m.path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	for index := 0; index < len(ordered); {
		start := ordered[index]
		end := start + uint64(pageSize)
		index++
		for index < len(ordered) && ordered[index] <= end {
			candidateEnd := ordered[index] + uint64(pageSize)
			if candidateEnd > end {
				end = candidateEnd
			}
			index++
		}
		if end > uint64(m.metadata.FileSize) {
			end = uint64(m.metadata.FileSize)
		}
		physical := m.metadata.FileOffset + int64(start)
		if err := m.copyRange(ctx, file, physical, int64(end-start)); err != nil {
			return fmt.Errorf("fetch restore-critical memory range at %d: %w", start, err)
		}
	}
	return file.Sync()
}

func (m *LazyMemoryMaterializer) MaterializeAll(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("lazy memory materializer is nil")
	}
	m.lock.Lock()
	defer m.lock.Unlock()
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
