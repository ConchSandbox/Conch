package stratovirt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/vmm/driver"
)

func TestStratovirtBuildStartCmd(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "stratovirt")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	client := NewStratovirtClient(1, "/tmp/conch-qmp.sock")
	script, err := client.BuildStartCmd(&driver.ResourceArgs{
		CPUBoot:     2,
		CPUMax:      4,
		MemorySize:  1024,
		MemoryPath:  "/must/not/be/used/mem.img",
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
		"-m 1024M",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	for _, unwanted := range []string{"/must/not/be/used/mem.img", "-incoming", "memory-backend-file"} {
		if strings.Contains(script, unwanted) {
			t.Fatalf("cold script unexpectedly contains %q:\n%s", unwanted, script)
		}
	}
}

func TestStratovirtBuildResumeCmdUsesMappedCheckpoint(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "stratovirt")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	client := NewStratovirtClient(1, "/tmp/conch-qmp.sock")
	script, err := client.BuildStartCmd(&driver.ResourceArgs{
		CPUBoot:      1,
		CPUMax:       1,
		MemorySize:   256,
		NamespaceID:  "ns-test",
		TapName:      "tap0",
		KernelPath:   "/tmp/kernel",
		InitrdPath:   "/tmp/initrd",
		SnapfilePath: "/tmp/snapshot",
		MemoryPath:   "/must/not/be/used/mem.img",
		VsockCID:     42,
		SandboxId:    "sandbox-test",
	}, true)
	if err != nil {
		t.Fatalf("BuildStartCmd() error = %v", err)
	}

	if want := "-incoming file:/tmp/snapshot,mapped=true"; !strings.Contains(script, want) {
		t.Fatalf("resume script missing %q:\n%s", want, script)
	}
	if !strings.Contains(script, "-m 256M") {
		t.Fatalf("resume script is missing captured memory size:\n%s", script)
	}
	if strings.Contains(script, "/must/not/be/used/mem.img") {
		t.Fatalf("resume script consumed MemoryPath:\n%s", script)
	}
}

func TestStratovirtPrepareLaunchDoesNotConsumeCLHSnapshotConfig(t *testing.T) {
	client := NewStratovirtClient(1, filepath.Join(t.TempDir(), "qmp.sock"))
	if err := client.PrepareLaunch(&driver.ResourceArgs{
		SnapfilePath: filepath.Join(t.TempDir(), "conch", "snapshot"),
		MemoryPath:   filepath.Join(t.TempDir(), "mem.img"),
	}, true); err != nil {
		t.Fatalf("PrepareLaunch() error = %v", err)
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

func TestWaitForVmmSocketWaitsUntilPathExists(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = os.WriteFile(socketPath, []byte{}, 0644)
	}()

	if err := waitForVmmSocket(ctx, socketPath, nil); err != nil {
		t.Fatalf("waitForVmmSocket() error = %v", err)
	}
}

func TestWaitForVmmSocketReturnsProcessExitError(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	processErr := errors.New("stratovirt exited before creating qmp socket")
	processExited := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	processExited <- processErr
	close(processExited)

	err := waitForVmmSocket(ctx, socketPath, processExited)
	if !errors.Is(err, processErr) {
		t.Fatalf("waitForVmmSocket() error = %v, want %v", err, processErr)
	}
	if !strings.Contains(err.Error(), "exited before vmm socket") {
		t.Fatalf("waitForVmmSocket() error = %q, want early exit context", err.Error())
	}
}
