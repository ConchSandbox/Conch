package cow

import (
	"errors"
	"fmt"
	"io"
	"os"
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

type probeOps struct {
	pageSize    func() int
	userfaultfd func(flags int) (int, error)
	ioctlAPI    func(fd int, api *uffdioAPI) error
	openPagemap func() (io.ReadCloser, error)
	closeFD     func(fd int) error
}

func productionProbeOps() probeOps {
	return probeOps{
		pageSize: os.Getpagesize,
		userfaultfd: func(flags int) (int, error) {
			fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, uintptr(flags), 0, 0)
			if errno != 0 {
				return -1, errno
			}
			return int(fd), nil
		},
		ioctlAPI: func(fd int, api *uffdioAPI) error {
			_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uffdioAPIRequest, uintptr(unsafe.Pointer(api)))
			if errno != 0 {
				return errno
			}
			return nil
		},
		openPagemap: func() (io.ReadCloser, error) { return os.Open("/proc/self/pagemap") },
		closeFD:     unix.Close,
	}
}

func ProbeCapabilities() Capabilities {
	return probeCapabilities(productionProbeOps())
}

func probeCapabilities(ops probeOps) Capabilities {
	fd, err := ops.userfaultfd(unix.O_CLOEXEC | unix.O_NONBLOCK)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) {
			return unsupportedCapabilities(requiredUFFDFeatureNames)
		}
		return unknownCapabilities("create userfaultfd", err)
	}
	closed := false
	closeUFFD := func() error {
		if closed {
			return nil
		}
		closed = true
		return ops.closeFD(fd)
	}

	api := uffdioAPI{API: uffdAPI}
	if err := ops.ioctlAPI(fd, &api); err != nil {
		closeErr := closeUFFD()
		if errors.Is(err, unix.ENOSYS) && closeErr == nil {
			return unsupportedCapabilities(requiredUFFDFeatureNames)
		}
		return unknownCapabilities("negotiate UFFDIO_API", errors.Join(err, closeErr))
	}
	if api.API != uffdAPI {
		return unknownCapabilities("negotiate UFFDIO_API", errors.Join(fmt.Errorf("kernel returned API %#x", api.API), closeUFFD()))
	}
	if missing := missingUFFDFeatures(api.Features); len(missing) != 0 {
		if err := closeUFFD(); err != nil {
			return unknownCapabilities("close temporary userfaultfd", err)
		}
		return unsupportedCapabilities(missing)
	}
	pageSize := ops.pageSize()
	if pageSize <= 0 || !compatibleHostPageSize(uint64(pageSize), memsnap.DefaultBlockSize) {
		if err := closeUFFD(); err != nil {
			return unknownCapabilities("close temporary userfaultfd", err)
		}
		return unsupportedCapabilities([]string{hostPageSizeFeatureName})
	}
	pagemap, err := ops.openPagemap()
	if err != nil {
		return unknownPagemapCapabilities("open pagemap", errors.Join(err, closeUFFD()))
	}
	var entry [8]byte
	_, readErr := io.ReadFull(pagemap, entry[:])
	closeErr := errors.Join(pagemap.Close(), closeUFFD())
	if readErr != nil {
		return unknownPagemapCapabilities("read pagemap", errors.Join(readErr, closeErr))
	}
	if closeErr != nil {
		return unknownCapabilities("close probe descriptors", closeErr)
	}
	return Capabilities{IncrementalMemory: CapabilitySupported}
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

func unsupportedCapabilities(missing []string) Capabilities {
	return Capabilities{IncrementalMemory: CapabilityUnsupported, MissingFeatures: append([]string(nil), missing...)}
}

func unknownCapabilities(operation string, err error) Capabilities {
	return Capabilities{IncrementalMemory: CapabilityUnknown, ProbeError: fmt.Sprintf("%s: %v", operation, err)}
}

func unknownPagemapCapabilities(operation string, err error) Capabilities {
	result := unknownCapabilities(operation, err)
	result.MissingFeatures = []string{pagemapFeatureName}
	return result
}

func compatibleHostPageSize(pageSize, blockSize uint64) bool {
	return pageSize != 0 && pageSize&(pageSize-1) == 0 && pageSize >= blockSize && pageSize%blockSize == 0
}
