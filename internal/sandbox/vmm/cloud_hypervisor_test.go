package vmm

import (
	"strings"
	"testing"
)

func TestBuildPmemArgsRepeatsCloudHypervisorOption(t *testing.T) {
	got := buildPmemArgs([]string{
		"/var/lib/conch/rootfs/layer0.erofs",
		" ",
		"/var/lib/conch/rootfs/layer1.erofs",
	})

	if count := strings.Count(got, "--pmem file="); count != 2 {
		t.Fatalf("pmem option count = %d, want 2 in %q", count, got)
	}
	if strings.Contains(got, ",discard_writes=on file=") {
		t.Fatalf("pmem layers were collapsed into one option: %q", got)
	}
	if !strings.Contains(got, "\\\n--pmem file=/var/lib/conch/rootfs/layer1.erofs,discard_writes=on") {
		t.Fatalf("pmem arguments are not line-continuation separated: %q", got)
	}
}
