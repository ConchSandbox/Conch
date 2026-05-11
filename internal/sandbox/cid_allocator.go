package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/pkg/ulog"
	"golang.org/x/sys/unix"
)

const (
	CIDMapFile     = "cidmap.json"
	MinCID         = 3
	MaxCID         = uint32(4294967294) // uint32 max - 1, 预留一个避免边界问题
	CIDMapFilePerm = 0644
)

type CIDMap struct {
	NextCID uint32            `json:"next_cid"`
	CidMap  map[string]uint32 `json:"cid_map"` // sandboxId -> cid
}

type CIDAllocator struct {
	mu       sync.Mutex
	dirPath  string
	filePath string
	usedCIDs map[uint32]bool // reverse index: cid -> in-use, for O(1) lookup
}

func NewCIDAllocator() *CIDAllocator {
	dirPath := filepath.Join(config.WorkDir, "maptable")
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		ulog.Error("failed to create cidmap directory", ulog.F("error", err))
	}

	filePath := filepath.Join(dirPath, CIDMapFile)
	allocator := &CIDAllocator{
		dirPath:  dirPath,
		filePath: filePath,
		usedCIDs: make(map[uint32]bool),
	}

	allocator.cleanupTempFiles()

	if err := allocator.initFile(); err != nil {
		ulog.Error("failed to init cidmap file", ulog.F("error", err))
	}

	return allocator
}

func (a *CIDAllocator) cleanupTempFiles() {
	matches, err := filepath.Glob(filepath.Join(a.dirPath, ".cidmap-*.tmp"))
	if err != nil {
		ulog.Warn("failed to glob temp files", ulog.F("error", err))
		return
	}
	for _, tmpPath := range matches {
		if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
			ulog.Warn("failed to remove stale temp file", ulog.F("path", tmpPath), ulog.F("error", err))
		}
	}
}

func (a *CIDAllocator) initFile() error {
	if _, err := os.Stat(a.filePath); os.IsNotExist(err) {
		initialMap := CIDMap{
			NextCID: MinCID,
			CidMap:  make(map[string]uint32),
		}
		return a.writeCIDMap(&initialMap)
	}

	// Build reverse index from existing file.
	cidMap, err := a.readCIDMap()
	if err != nil {
		return err
	}
	for _, cid := range cidMap.CidMap {
		a.usedCIDs[cid] = true
	}

	return nil
}

func (a *CIDAllocator) readCIDMap() (*CIDMap, error) {
	data, err := os.ReadFile(a.filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read cidmap file: %w", err)
	}

	if len(data) == 0 {
		return &CIDMap{
			NextCID: MinCID,
			CidMap:  make(map[string]uint32),
		}, nil
	}

	var cidMap CIDMap
	if err := json.Unmarshal(data, &cidMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal cidmap: %w", err)
	}

	if cidMap.CidMap == nil {
		cidMap.CidMap = make(map[string]uint32)
	}

	return &cidMap, nil
}

func (a *CIDAllocator) writeCIDMap(cidMap *CIDMap) error {
	data, err := json.MarshalIndent(cidMap, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal cidmap: %w", err)
	}
	return a.atomicWrite(data)
}

func (a *CIDAllocator) atomicWrite(data []byte) error {
	dir := filepath.Dir(a.filePath)
	tmpFile, err := os.CreateTemp(dir, ".cidmap-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to sync temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, a.filePath); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

func (a *CIDAllocator) acquireFileLock() (int, error) {
	fd, err := unix.Open(a.filePath, unix.O_RDWR|unix.O_CREAT, CIDMapFilePerm)
	if err != nil {
		return -1, fmt.Errorf("failed to open cidmap file: %w", err)
	}

	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("failed to acquire exclusive lock: %w", err)
	}

	return fd, nil
}

func (a *CIDAllocator) acquireFileLockShared() (int, error) {
	fd, err := unix.Open(a.filePath, unix.O_RDONLY|unix.O_CREAT, CIDMapFilePerm)
	if err != nil {
		return -1, fmt.Errorf("failed to open cidmap file: %w", err)
	}

	if err := unix.Flock(fd, unix.LOCK_SH); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("failed to acquire shared lock: %w", err)
	}

	return fd, nil
}

func (a *CIDAllocator) releaseFileLock(fd int) {
	unix.Flock(fd, unix.LOCK_UN)
	unix.Close(fd)
}

func (a *CIDAllocator) AllocateCID(sandboxId string) (uint32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	fd, err := a.acquireFileLock()
	if err != nil {
		return 0, err
	}
	defer a.releaseFileLock(fd)

	cidMap, err := a.readCIDMap()
	if err != nil {
		return 0, err
	}

	if existingCID, ok := cidMap.CidMap[sandboxId]; ok {
		return existingCID, nil
	}

	// Find next available CID using reverse index for O(1) lookup.
	cid := cidMap.NextCID
	tries := uint32(0)
	for {
		if cid > MaxCID {
			cid = MinCID // wrap around
		}
		if !a.usedCIDs[cid] {
			break
		}
		cid++
		tries++
		if tries > MaxCID-MinCID {
			return 0, fmt.Errorf("CID allocation failed: no available CIDs. Please cleanup old sandboxes to release CIDs. Current active sandboxes: %d",
				len(cidMap.CidMap))
		}
	}

	cidMap.CidMap[sandboxId] = cid
	nextCID := cid + 1
	if nextCID > MaxCID {
		nextCID = MinCID
	}
	cidMap.NextCID = nextCID
	a.usedCIDs[cid] = true

	if err := a.writeCIDMap(cidMap); err != nil {
		delete(a.usedCIDs, cid)
		delete(cidMap.CidMap, sandboxId)
		return 0, err
	}

	ulog.Info("allocated CID", ulog.F("sandbox_id", sandboxId), ulog.F("cid", cid))
	return cid, nil
}

func (a *CIDAllocator) ReleaseCID(sandboxId string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	fd, err := a.acquireFileLock()
	if err != nil {
		return err
	}
	defer a.releaseFileLock(fd)

	cidMap, err := a.readCIDMap()
	if err != nil {
		return err
	}

	if cid, ok := cidMap.CidMap[sandboxId]; ok {
		delete(a.usedCIDs, cid)
	}
	delete(cidMap.CidMap, sandboxId)

	return a.writeCIDMap(cidMap)
}

func (a *CIDAllocator) GetCID(sandboxId string) (uint32, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	fd, err := a.acquireFileLockShared()
	if err != nil {
		return 0, false
	}
	defer a.releaseFileLock(fd)

	cidMap, err := a.readCIDMap()
	if err != nil {
		return 0, false
	}

	cid, ok := cidMap.CidMap[sandboxId]
	return cid, ok
}

func (a *CIDAllocator) GetActiveCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	fd, err := a.acquireFileLockShared()
	if err != nil {
		return 0
	}
	defer a.releaseFileLock(fd)

	cidMap, err := a.readCIDMap()
	if err != nil {
		return 0
	}

	return len(cidMap.CidMap)
}

func (a *CIDAllocator) ListActiveSandboxes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	fd, err := a.acquireFileLockShared()
	if err != nil {
		return nil
	}
	defer a.releaseFileLock(fd)

	cidMap, err := a.readCIDMap()
	if err != nil {
		return nil
	}

	sandboxIds := make([]string, 0, len(cidMap.CidMap))
	for sandboxId := range cidMap.CidMap {
		sandboxIds = append(sandboxIds, sandboxId)
	}
	return sandboxIds
}

func (a *CIDAllocator) Cleanup() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.usedCIDs = make(map[uint32]bool)

	if err := os.Remove(a.filePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove cidmap file: %w", err)
	}
	return nil
}
