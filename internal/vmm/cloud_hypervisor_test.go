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
}
