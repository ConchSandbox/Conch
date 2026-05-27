package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/openeuler/Conch/pkg/ulog"
	"golang.org/x/sys/unix"
)

const (
	CIDMapDir      = "/var/run/conch/maptable"
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
	filePath string
}

func NewCIDAllocator() *CIDAllocator {
	if err := os.MkdirAll(CIDMapDir, 0755); err != nil {
		ulog.Error("failed to create cidmap directory", ulog.F("error", err))
	}

	filePath := filepath.Join(CIDMapDir, CIDMapFile)
	allocator := &CIDAllocator{
		filePath: filePath,
	}

	if err := allocator.initFile(); err != nil {
		ulog.Error("failed to init cidmap file", ulog.F("error", err))
	}

	return allocator
}

func (a *CIDAllocator) initFile() error {
	if _, err := os.Stat(a.filePath); os.IsNotExist(err) {
		initialMap := CIDMap{
			NextCID: MinCID,
			CidMap:  make(map[string]uint32),
		}
		return a.writeCIDMap(&initialMap)
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

	return os.WriteFile(a.filePath, data, CIDMapFilePerm)
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

	cidMap, err := a.readCIDMapLocked(fd)
	if err != nil {
		return 0, err
	}

	if existingCID, ok := cidMap.CidMap[sandboxId]; ok {
		return existingCID, nil
	}

	if cidMap.NextCID > MaxCID {
		return 0, fmt.Errorf("CID allocation failed: reached maximum CID limit (%d). Please cleanup old sandboxes to release CIDs. Current active sandboxes: %d",
			MaxCID, len(cidMap.CidMap))
	}

	cid := cidMap.NextCID
	cidMap.CidMap[sandboxId] = cid
	cidMap.NextCID = cid + 1

	if err := a.writeCIDMapLocked(fd, cidMap); err != nil {
		return 0, err
	}

	ulog.Info("allocated CID", ulog.F("sandbox_id", sandboxId), ulog.F("cid", cid))
	return cid, nil
}

func (a *CIDAllocator) readCIDMapLocked(fd int) (*CIDMap, error) {
	if _, err := unix.Seek(fd, 0, 0); err != nil {
		return nil, fmt.Errorf("failed to seek to beginning: %w", err)
	}

	var buf []byte
	for {
		chunk := make([]byte, 1024)
		n, err := unix.Read(fd, chunk)
		if err != nil {
			return nil, fmt.Errorf("failed to read cidmap: %w", err)
		}
		if n == 0 {
			break
		}
		buf = append(buf, chunk[:n]...)
	}

	if len(buf) == 0 {
		return &CIDMap{
			NextCID: MinCID,
			CidMap:  make(map[string]uint32),
		}, nil
	}

	var cidMap CIDMap
	if err := json.Unmarshal(buf, &cidMap); err != nil {
		return nil, fmt.Errorf("failed to unmarshal cidmap: %w", err)
	}

	if cidMap.CidMap == nil {
		cidMap.CidMap = make(map[string]uint32)
	}

	return &cidMap, nil
}

func (a *CIDAllocator) writeCIDMapLocked(fd int, cidMap *CIDMap) error {
	data, err := json.MarshalIndent(cidMap, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal cidmap: %w", err)
	}

	if _, err := unix.Seek(fd, 0, 0); err != nil {
		return fmt.Errorf("failed to seek: %w", err)
	}

	if err := unix.Ftruncate(fd, 0); err != nil {
		return fmt.Errorf("failed to truncate: %w", err)
	}

	if _, err := unix.Write(fd, data); err != nil {
		return fmt.Errorf("failed to write cidmap: %w", err)
	}

	return nil
}

func (a *CIDAllocator) ReleaseCID(sandboxId string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	fd, err := a.acquireFileLock()
	if err != nil {
		return err
	}
	defer a.releaseFileLock(fd)

	cidMap, err := a.readCIDMapLocked(fd)
	if err != nil {
		return err
	}

	delete(cidMap.CidMap, sandboxId)

	return a.writeCIDMapLocked(fd, cidMap)
}

func (a *CIDAllocator) ReserveCID(sandboxId string, cid uint32) error {
	if sandboxId == "" || cid == 0 {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	fd, err := a.acquireFileLock()
	if err != nil {
		return err
	}
	defer a.releaseFileLock(fd)

	cidMap, err := a.readCIDMapLocked(fd)
	if err != nil {
		return err
	}
	if existing, ok := cidMap.CidMap[sandboxId]; ok && existing != cid {
		return fmt.Errorf("sandbox %s already has cid %d, cannot reserve %d", sandboxId, existing, cid)
	}
	for id, existing := range cidMap.CidMap {
		if id != sandboxId && existing == cid {
			return fmt.Errorf("cid %d already reserved by sandbox %s", cid, id)
		}
	}
	cidMap.CidMap[sandboxId] = cid
	if cid >= cidMap.NextCID {
		cidMap.NextCID = cid + 1
	}
	return a.writeCIDMapLocked(fd, cidMap)
}

func (a *CIDAllocator) GetCID(sandboxId string) (uint32, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	fd, err := a.acquireFileLock()
	if err != nil {
		return 0, false
	}
	defer a.releaseFileLock(fd)

	cidMap, err := a.readCIDMapLocked(fd)
	if err != nil {
		return 0, false
	}

	cid, ok := cidMap.CidMap[sandboxId]
	return cid, ok
}

func (a *CIDAllocator) GetActiveCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	fd, err := a.acquireFileLock()
	if err != nil {
		return 0
	}
	defer a.releaseFileLock(fd)

	cidMap, err := a.readCIDMapLocked(fd)
	if err != nil {
		return 0
	}

	return len(cidMap.CidMap)
}

func (a *CIDAllocator) ListActiveSandboxes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	fd, err := a.acquireFileLock()
	if err != nil {
		return nil
	}
	defer a.releaseFileLock(fd)

	cidMap, err := a.readCIDMapLocked(fd)
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

	if err := os.Remove(a.filePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove cidmap file: %w", err)
	}
	return nil
}
