package vmm

import (
	"strings"
	"testing"
)

func TestBuildPmemArgsCloudHypervisorOption(t *testing.T) {
	got := buildPmemArgs([]string{
		"/var/lib/conch/rootfs/layer0.erofs",
		" ",
		"/var/lib/conch/rootfs/layer1.erofs",
	})

	if count := strings.Count(got, "--pmem"); count != 1 {
		t.Fatalf("pmem option count = %d, want 1 in %q", count, got)
	}
	if !strings.Contains(got, "--pmem \\\nfile=/var/lib/conch/rootfs/layer0.erofs,discard_writes=on") {
		t.Fatalf("first pmem file missing from single option: %q", got)
	}
	if !strings.Contains(got, "\\\nfile=/var/lib/conch/rootfs/layer1.erofs,discard_writes=on") {
		t.Fatalf("pmem files are not line-continuation separated: %q", got)
	}
	if strings.Contains(got, "pci_segment=") {
		t.Fatalf("small pmem set should not set pci_segment: %q", got)
	}
}

func TestBuildPmemArgsDistributesLargePmemSetAcrossPciSegments(t *testing.T) {
	paths := make([]string, 0, 25)
	for i := 0; i < 25; i++ {
		paths = append(paths, "/var/lib/conch/rootfs/layer"+string(rune('a'+i))+".erofs")
	}

	got := buildPmemArgs(paths)
	if !strings.Contains(got, "layera.erofs,discard_writes=on,pci_segment=0") {
		t.Fatalf("first pmem device should use pci segment 0: %q", got)
	}
	if !strings.Contains(got, "layery.erofs,discard_writes=on,pci_segment=1") {
		t.Fatalf("25th pmem device should use pci segment 1: %q", got)
	}
}

func TestBuildPlatformArgsAddsSegmentsForLargePmemSet(t *testing.T) {
	paths := make([]string, 25)
	for i := range paths {
		paths[i] = "/var/lib/conch/rootfs/layer.erofs"
	}

	got := buildPlatformArgs(paths)
	if got != `--platform "num_pci_segments=2"` {
		t.Fatalf("buildPlatformArgs() = %q, want 2 segments", got)
	}
}
