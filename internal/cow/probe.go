package cow

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unsafe"

	"github.com/openeuler/Conch/internal/memsnap"
	"golang.org/x/sys/unix"
)

const (
	uffdAPI                     = uint64(0xaa)
	uffdFeaturePagefaultFlagWP  = uint64(1 << 0)
	uffdFeatureMissingShmem     = uint64(1 << 5)
	uffdFeatureWPHugetlbfsShmem = uint64(1 << 12)
	uffdFeatureWPAsync          = uint64(1 << 15)
	uffdioAPIRequest            = uintptr(0xc018aa3f)
)

const requiredUFFDFeatures = uffdFeatureMissingShmem |
	uffdFeaturePagefaultFlagWP |
	uffdFeatureWPHugetlbfsShmem |
	uffdFeatureWPAsync

var requiredUFFDFeatureNames = []string{
	"uffd.missing_shmem",
	"uffd.pagefault_flag_wp",
	"uffd.wp_hugetlbfs_shmem",
	"uffd.wp_async",
}

const (
	pagemapFeatureName      = "pagemap.read"
	hostPageSizeFeatureName = "host-page-size.snapshot-block-compatible"
)

type uffdioAPI struct {
	API      uint64
	Features uint64
	Ioctls   uint64
}

func ProbeIncrementalMemory() error {
	rawFD, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, uintptr(unix.O_CLOEXEC|unix.O_NONBLOCK), 0, 0)
	if errno != 0 {
		if errors.Is(errno, unix.ENOSYS) {
			return missingFeaturesError(requiredUFFDFeatureNames)
		}
		return fmt.Errorf("create userfaultfd: %w", errno)
	}
	fd := int(rawFD)
	closed := false
	closeUFFD := func() error {
		if closed {
			return nil
		}
		closed = true
		return unix.Close(fd)
	}

	api := uffdioAPI{API: uffdAPI}
	_, _, errno = unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uffdioAPIRequest, uintptr(unsafe.Pointer(&api)))
	if errno != 0 {
		closeErr := closeUFFD()
		if errors.Is(errno, unix.ENOSYS) && closeErr == nil {
			return missingFeaturesError(requiredUFFDFeatureNames)
		}
		return fmt.Errorf("negotiate UFFDIO_API: %w", errors.Join(errno, closeErr))
	}
	if api.API != uffdAPI {
		return fmt.Errorf("negotiate UFFDIO_API: %w", errors.Join(fmt.Errorf("kernel returned API %#x", api.API), closeUFFD()))
	}
	if missing := missingUFFDFeatures(api.Features); len(missing) != 0 {
		if err := closeUFFD(); err != nil {
			return fmt.Errorf("close temporary userfaultfd: %w", err)
		}
		return missingFeaturesError(missing)
	}
	pageSize := os.Getpagesize()
	if pageSize <= 0 || !compatibleHostPageSize(uint64(pageSize), memsnap.DefaultBlockSize) {
		if err := closeUFFD(); err != nil {
			return fmt.Errorf("close temporary userfaultfd: %w", err)
		}
		return missingFeaturesError([]string{hostPageSizeFeatureName})
	}
	pagemap, err := os.Open("/proc/self/pagemap")
	if err != nil {
		return fmt.Errorf("probe %s: %w", pagemapFeatureName, errors.Join(err, closeUFFD()))
	}
	var entry [8]byte
	_, readErr := io.ReadFull(pagemap, entry[:])
	closeErr := errors.Join(pagemap.Close(), closeUFFD())
	if readErr != nil {
		return fmt.Errorf("probe %s: %w", pagemapFeatureName, errors.Join(readErr, closeErr))
	}
	if closeErr != nil {
		return fmt.Errorf("close probe descriptors: %w", closeErr)
	}
	return nil
}

func missingUFFDFeatures(features uint64) []string {
	tests := []struct {
		bit  uint64
		name string
	}{
		{uffdFeatureMissingShmem, "uffd.missing_shmem"},
		{uffdFeaturePagefaultFlagWP, "uffd.pagefault_flag_wp"},
		{uffdFeatureWPHugetlbfsShmem, "uffd.wp_hugetlbfs_shmem"},
		{uffdFeatureWPAsync, "uffd.wp_async"},
	}
	var missing []string
	for _, feature := range tests {
		if features&feature.bit == 0 {
			missing = append(missing, feature.name)
		}
	}
	return missing
}

func missingFeaturesError(missing []string) error {
	return fmt.Errorf("missing incremental memory features: %s", strings.Join(missing, ", "))
}

func compatibleHostPageSize(pageSize, blockSize uint64) bool {
	return pageSize != 0 && pageSize&(pageSize-1) == 0 && pageSize >= blockSize && pageSize%blockSize == 0
}
