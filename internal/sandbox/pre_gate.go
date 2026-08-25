package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/openeuler/Conch/pkg/ulog"
	"golang.org/x/sys/unix"
)

const (
	preGateProfileVersion = 1
	resumeGateSize        = 4
	futexWake             = 1
	maxFutexWake          = 0x7fffffff
)

type preGateProfile struct {
	Version  int      `json:"version"`
	PageSize int64    `json:"page_size"`
	FileSize int64    `json:"file_size"`
	Offsets  []uint64 `json:"offsets"`
}

func configurePreGate(ctx context.Context, spec *VMStartSpec, sandboxID, stateDir string) error {
	if spec == nil || !spec.PreGateRequired || strings.TrimSpace(spec.SnapfilePath) == "" || strings.TrimSpace(spec.PreGateKey) == "" {
		return nil
	}
	memoryPath := filepath.Join(spec.SnapfilePath, "memory")
	info, err := os.Stat(memoryPath)
	if err != nil {
		return fmt.Errorf("stat snapshot memory: %w", err)
	}
	t0 := time.Now()
	resident, err := memoryFullyResident(memoryPath, info.Size())
	if err != nil {
		return fmt.Errorf("check snapshot memory residency: %w", err)
	}
	ulog.GetLogger().Debug("pre-gate phase memoryFullyResident", ulog.F("sandbox", sandboxID), ulog.F("elapsed", time.Since(t0)))
	if resident {
		ulog.GetLogger().Debug("Skipping pre-gate for fully resident snapshot memory", ulog.F("path", memoryPath))
		return nil
	}
	profilePath := filepath.Join(stateDir, profileFileName(spec.PreGateKey))
	var profile *preGateProfile
	if len(spec.PreGateProfile) != 0 {
		profile, err = decodePreGateProfile(spec.PreGateProfile, info.Size())
		if err != nil {
			return fmt.Errorf("validate portable pre-gate profile: %w", err)
		}
		if mkdirErr := os.MkdirAll(stateDir, 0o700); mkdirErr != nil {
			return fmt.Errorf("create pre-gate state directory: %w", mkdirErr)
		}
		if writeErr := os.WriteFile(profilePath, spec.PreGateProfile, 0o600); writeErr != nil {
			return fmt.Errorf("cache portable pre-gate profile: %w", writeErr)
		}
	} else {
		profile, err = loadPreGateProfile(profilePath, info.Size())
	}
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			ulog.GetLogger().Warn("Ignoring invalid pre-gate profile", ulog.F("path", profilePath), ulog.F("error", err))
		}
		// No restore set is known yet, so starting the VMM against a partially
		// materialized file would be unsafe. Pay the full sequential read cost
		// before this restore and use it to learn the next one.
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return fmt.Errorf("create pre-gate state directory: %w", err)
		}
		materialize := spec.MaterializeAll
		if materialize == nil {
			materialize = func(ctx context.Context) error { return warmWholeFile(ctx, memoryPath) }
		}
		if err := materialize(ctx); err != nil {
			return fmt.Errorf("materialize snapshot memory before recording pre-gate profile: %w", err)
		}
		// Record-only restores preserve the existing launch path. Dropping cached
		// pages is best-effort and keeps the learned set representative of restore.
		if file, openErr := os.Open(memoryPath); openErr == nil {
			_ = unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_DONTNEED)
			_ = file.Close()
		}
		spec.RecordPreGatePath = profilePath
		return nil
	}

	if spec.MaterializeCritical != nil {
		t1 := time.Now()
		maxOffset := uint64(0)
		for _, off := range profile.Offsets {
			if off > maxOffset {
				maxOffset = off
			}
		}
		top := maxOffset + uint64(1024*1024)
		if top > uint64(profile.FileSize) {
			top = uint64(profile.FileSize)
		}
		window := make([]uint64, 0, int(top/uint64(profile.PageSize))+1)
		for off := uint64(0); off < top; off += uint64(profile.PageSize) {
			window = append(window, off)
		}
		if err := spec.MaterializeCritical(ctx, profile.PageSize, window); err != nil {
			return fmt.Errorf("materialize restore-critical memory: %w", err)
		}
		ulog.GetLogger().Debug("pre-gate phase MaterializeCritical", ulog.F("sandbox", sandboxID), ulog.F("elapsed", time.Since(t1)), ulog.F("window_bytes", top))
	}
	t2 := time.Now()
	if err := warmProfile(memoryPath, profile); err != nil {
		return fmt.Errorf("warm pre-gate profile: %w", err)
	}
	ulog.GetLogger().Debug("pre-gate phase warmProfile", ulog.F("sandbox", sandboxID), ulog.F("elapsed", time.Since(t2)))
	gateDir := filepath.Join(stateDir, "gates")
	if err := os.MkdirAll(gateDir, 0o700); err != nil {
		return fmt.Errorf("create resume gate directory: %w", err)
	}
	gatePath := filepath.Join(gateDir, hashedName(sandboxID)+".gate")
	if err := os.WriteFile(gatePath, make([]byte, resumeGateSize), 0o600); err != nil {
		return fmt.Errorf("create resume gate: %w", err)
	}
	spec.ResumeGatePath = gatePath
	go func() {
		materialize := spec.MaterializeAll
		if materialize == nil {
			materialize = func(ctx context.Context) error { return warmWholeFile(ctx, memoryPath) }
		}
		if warmErr := materialize(ctx); warmErr != nil {
			// Keep the gate closed: resuming with an incomplete backing file can
			// expose stale or zero-filled guest memory.
			ulog.GetLogger().Error("Full pre-gate memory materialization failed", ulog.F("error", warmErr))
			return
		}
		// The EROFS view cached zero-filled hole pages while the backing was
		// still sparse (restore-time readaround). Drop the EROFS page cache for
		// the memory file so the guest reads the verified backing data.
		if dropErr := dropErofsMemoryCache(memoryPath); dropErr != nil {
			ulog.GetLogger().Warn("Failed to drop pre-gate EROFS cache", ulog.F("error", dropErr))
		}
		if signalErr := signalResumeGate(gatePath); signalErr != nil {
			ulog.GetLogger().Error("Failed to signal resume gate", ulog.F("error", signalErr))
		}
		if spec.MaterializeCommit != nil {
			if commitErr := spec.MaterializeCommit(); commitErr != nil {
				ulog.GetLogger().Warn("Failed to commit lazy memory layer", ulog.F("error", commitErr))
			}
		}
	}()
	return nil
}

// dropErofsMemoryCache drops the page cache of a memory file exposed through an
// EROFS mount. While the lazy backing was still sparse, restore-time reads (and
// EROFS readaround) cached zero-filled hole pages that are not invalidated by
// later writes to the backing file; dropping them makes the guest read the
// verified data instead of stale zeros.
func dropErofsMemoryCache(memoryPath string) error {
	file, err := os.Open(memoryPath)
	if err != nil {
		return err
	}
	defer file.Close()
	return unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_DONTNEED)
}

func memoryFullyResident(path string, fileSize int64) (bool, error) {
	if fileSize <= 0 {
		return true, nil
	}
	if uint64(fileSize) > uint64(^uint(0)>>1) {
		return false, fmt.Errorf("snapshot memory is too large to map: %d", fileSize)
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	mapping, err := syscall.Mmap(int(file.Fd()), 0, int(fileSize), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return false, err
	}
	defer syscall.Munmap(mapping)

	pageSize := os.Getpagesize()
	residency := make([]byte, (len(mapping)+pageSize-1)/pageSize)
	_, _, errno := syscall.Syscall(
		syscall.SYS_MINCORE,
		uintptr(unsafe.Pointer(&mapping[0])),
		uintptr(len(mapping)),
		uintptr(unsafe.Pointer(&residency[0])),
	)
	if errno != 0 {
		return false, errno
	}
	for _, value := range residency {
		if value&1 == 0 {
			return false, nil
		}
	}
	return true, nil
}

func profileFileName(key string) string {
	return hashedName(strings.TrimSpace(key)) + ".json"
}

func PreGateProfilePath(stateDir, key string) string {
	return filepath.Join(stateDir, profileFileName(key))
}

func hashedName(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:16])
}

func loadPreGateProfile(path string, fileSize int64) (*preGateProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decodePreGateProfile(data, fileSize)
}

func decodePreGateProfile(data []byte, fileSize int64) (*preGateProfile, error) {
	var profile preGateProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return nil, err
	}
	if profile.Version != preGateProfileVersion || profile.PageSize != int64(os.Getpagesize()) || profile.FileSize != fileSize {
		return nil, fmt.Errorf("profile identity does not match snapshot memory")
	}
	for _, offset := range profile.Offsets {
		if offset >= uint64(fileSize) || offset%uint64(profile.PageSize) != 0 {
			return nil, fmt.Errorf("invalid page offset %d", offset)
		}
	}
	return &profile, nil
}

func warmProfile(path string, profile *preGateProfile) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	page := make([]byte, profile.PageSize)
	for _, offset := range profile.Offsets {
		if _, err := file.ReadAt(page, int64(offset)); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	return nil
}

func warmWholeFile(ctx context.Context, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	buffer := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := file.Read(buffer)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		runtime.Gosched()
	}
}

func signalResumeGate(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	mapping, err := syscall.Mmap(int(file.Fd()), 0, resumeGateSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	defer syscall.Munmap(mapping)
	flag := (*uint32)(unsafe.Pointer(&mapping[0]))
	atomic.StoreUint32(flag, 1)
	_, _, errno := syscall.Syscall6(syscall.SYS_FUTEX, uintptr(unsafe.Pointer(flag)), futexWake, maxFutexWake, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func cleanupResumeGate(path string) {
	if strings.TrimSpace(path) != "" {
		_ = os.Remove(path)
	}
}
