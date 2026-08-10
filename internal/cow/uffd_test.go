package cow

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/memsnap"
	"golang.org/x/sys/unix"
)

func TestReceiveUFFDHandoffAcceptsStratoVirtMappingShapes(t *testing.T) {
	shapes := map[string]string{
		"direct":  `[{"base_host_virt_addr":65536,"size":4096,"offset":0,"page_size_kib":4096},{"base_host_virt_addr":131072,"size":4096,"offset":4096,"page_size_kib":4096}]`,
		"wrapped": `{"mappings":[{"base-host-virt-addr":65536,"size":4096,"offset":0,"page-size":4096},{"base-host-virt-addr":131072,"size":4096,"offset":4096,"page-size":4096}]}`,
	}
	for name, payload := range shapes {
		t.Run(name, func(t *testing.T) {
			receiver, sender := testUnixSocketPair(t)
			readEnd, writeEnd, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer readEnd.Close()
			defer writeEnd.Close()
			if _, _, err := sender.WriteMsgUnix([]byte(payload), unix.UnixRights(int(readEnd.Fd())), nil); err != nil {
				t.Fatal(err)
			}
			uffd, ranges, err := receiveUFFDHandoffForHostPage(receiver, 2*memsnap.DefaultBlockSize, memsnap.DefaultBlockSize, memsnap.DefaultBlockSize)
			if err != nil {
				t.Fatal(err)
			}
			defer uffd.Close()
			if len(ranges) != 2 || ranges[0].GuestOffset != 0 || ranges[1].GuestOffset != memsnap.DefaultBlockSize {
				t.Fatalf("ranges = %#v", ranges)
			}
		})
	}
}

func TestDecodeUFFDRangesRejectsInvalidCoverage(t *testing.T) {
	tests := map[string]string{
		"empty":            `[]`,
		"HVA overlap":      `[{"base_host_virt_addr":65536,"size":8192,"offset":0,"page_size_kib":4096},{"base_host_virt_addr":69632,"size":4096,"offset":4096,"page_size_kib":4096}]`,
		"guest gap":        `[{"base_host_virt_addr":65536,"size":4096,"offset":0,"page_size_kib":4096},{"base_host_virt_addr":131072,"size":4096,"offset":8192,"page_size_kib":4096}]`,
		"incomplete guest": `[{"base_host_virt_addr":65536,"size":4096,"offset":0,"page_size_kib":4096}]`,
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeUFFDRanges([]byte(payload), 2*memsnap.DefaultBlockSize, memsnap.DefaultBlockSize, memsnap.DefaultBlockSize); err == nil {
				t.Fatal("decodeUFFDRanges accepted invalid ranges")
			}
		})
	}
}

func TestPageFaultWritesBuildMapOwnerToMemfdBeforeWake(t *testing.T) {
	const hostPageSize = uint64(64 * 1024)
	root := t.TempDir()
	manifest, err := memsnap.CreateBaseLayer(root, hostPageSize, memsnap.DefaultBlockSize, func(sink memsnap.PageSink) error {
		for offset := uint64(0); offset < hostPageSize; offset += memsnap.DefaultBlockSize {
			page := bytes.Repeat([]byte{byte(offset/memsnap.DefaultBlockSize + 1)}, int(memsnap.DefaultBlockSize))
			if err := sink.WritePage(offset, page); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := memsnap.WriteManifestAtomic(filepath.Join(root, memsnap.ManifestFileName), manifest); err != nil {
		t.Fatal(err)
	}
	server := newServer(filepath.Join(t.TempDir(), "cow.sock"), Capabilities{IncrementalMemory: CapabilitySupported})
	server.uffdOps.acceptTimeout = time.Second
	response, fds := server.handleAttach(Request{SandboxID: "sandbox", MemorySnapshotRoot: root}, Response{})
	defer closeFDs(fds)
	defer server.Close()
	var wakeFD int
	var wakeStart, wakeLength uint64
	server.uffdOps.wake = func(fd int, start, length uint64) error {
		wakeFD, wakeStart, wakeLength = fd, start, length
		return nil
	}
	ranges := []uffdRange{{HVA: 0x20000, Length: hostPageSize, GuestOffset: 0, HostPageSize: hostPageSize}}
	item := server.attachments[response.Token]
	if err := server.handlePageFault(item, 9, 0x23456, ranges); err != nil {
		t.Fatal(err)
	}
	page := make([]byte, hostPageSize)
	read, err := unix.Pread(int(item.memfd.Fd()), page, 0)
	if err != nil {
		t.Fatal(err)
	}
	if read != int(hostPageSize) || page[0] != 1 || page[memsnap.DefaultBlockSize] != 2 {
		t.Fatalf("memfd size/bytes = %d/%d/%d", read, page[0], page[memsnap.DefaultBlockSize])
	}
	if wakeFD != 9 || wakeStart != 0x20000 || wakeLength != hostPageSize {
		t.Fatalf("wake = fd %d start %#x length %d", wakeFD, wakeStart, wakeLength)
	}
}

func TestPageFaultReturnsWakeFailure(t *testing.T) {
	const hostPageSize = uint64(memsnap.DefaultBlockSize)
	root := t.TempDir()
	manifest, err := memsnap.CreateBaseLayer(root, hostPageSize, memsnap.DefaultBlockSize, func(sink memsnap.PageSink) error {
		return sink.WriteZeroPage(0)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := memsnap.WriteManifestAtomic(filepath.Join(root, memsnap.ManifestFileName), manifest); err != nil {
		t.Fatal(err)
	}
	server := newServer(filepath.Join(t.TempDir(), "cow.sock"), Capabilities{IncrementalMemory: CapabilitySupported})
	server.uffdOps.acceptTimeout = time.Second
	response, fds := server.handleAttach(Request{SandboxID: "sandbox", MemorySnapshotRoot: root}, Response{})
	defer closeFDs(fds)
	defer server.Close()
	server.uffdOps.wake = func(int, uint64, uint64) error { return errors.New("wake failed") }
	ranges := []uffdRange{{HVA: 0x20000, Length: hostPageSize, GuestOffset: 0, HostPageSize: hostPageSize}}
	if err := server.handlePageFault(server.attachments[response.Token], 9, 0x20000, ranges); err == nil || !strings.Contains(err.Error(), "wake failed") {
		t.Fatalf("wake error = %v", err)
	}
}
