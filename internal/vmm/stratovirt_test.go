package vmm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewVmmClientCreatesStratovirtClient(t *testing.T) {
	client, err := newVmmClient(StratovirtVmmType, "/tmp/qmp.sock")
	if err != nil {
		t.Fatalf("newVmmClient() error = %v", err)
	}
	if _, ok := client.(*StratovirtClient); !ok {
		t.Fatalf("client type = %T, want *StratovirtClient", client)
	}
}

func TestStratovirtBuildStartCmd(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "stratovirt")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	client := NewStratovirtClient(StratovirtVmmType, "/tmp/conch-qmp.sock")
	script, err := client.BuildStartCmd(&ResourceArgs{
		CPUBoot:     2,
		CPUMax:      4,
		MemorySize:  1024,
		NamespaceID: "ns-test",
		TapName:     "tap0",
		KernelPath:  "/tmp/kernel",
		InitrdPath:  "/tmp/initrd",
		VsockCID:    42,
		SandboxId:   "sandbox-test",
	}, false)
	if err != nil {
		t.Fatalf("BuildStartCmd() error = %v", err)
	}

	for _, want := range []string{
		"ip netns exec ns-test",
		binPath,
		"-qmp unix:/tmp/conch-qmp.sock,server,nowait",
		"-device vhost-vsock-pci,id=vsock0,guest-cid=42",
		"conch.sandbox_id=sandbox-test",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}

func TestBuildStratovirtPmemDevices(t *testing.T) {
	pmemPath := filepath.Join(t.TempDir(), "layer.erofs")
	file, err := os.Create(pmemPath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := file.Truncate(2 * 1024 * 1024); err != nil {
		t.Fatalf("Truncate() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	got := buildStratovirtPmemDevices([]string{pmemPath})
	if !strings.Contains(got, "memory-backend-file,size=2M,id=pmem0,mem-path="+pmemPath) {
		t.Fatalf("pmem object missing: %q", got)
	}
	if !strings.Contains(got, "-device virtio-pmem-pci,id=pmem0pci,memdev=pmem0") {
		t.Fatalf("pmem device missing: %q", got)
	}
}
