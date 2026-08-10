package cow

import (
	"fmt"
	"os"
	"testing"

	"github.com/openeuler/Conch/internal/memsnap"
	"golang.org/x/sys/unix"
)

func TestReceiveUFFDHandoffAcceptsStratoVirtMappings(t *testing.T) {
	hostPageSize := uint64(os.Getpagesize())
	payload := fmt.Sprintf(
		`[{"base_host_virt_addr":%d,"size":%d,"offset":0,"page_size_kib":%d},{"base_host_virt_addr":%d,"size":%d,"offset":%d,"page_size_kib":%d}]`,
		hostPageSize, hostPageSize, hostPageSize,
		3*hostPageSize, hostPageSize, hostPageSize, hostPageSize,
	)
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
	uffd, ranges, err := receiveUFFDHandoff(receiver, 2*hostPageSize, memsnap.DefaultBlockSize)
	if err != nil {
		t.Fatal(err)
	}
	defer uffd.Close()
	if len(ranges) != 2 || ranges[0].GuestOffset != 0 || ranges[1].GuestOffset != hostPageSize {
		t.Fatalf("ranges = %#v", ranges)
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
