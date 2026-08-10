package stratovirt

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/vmm/driver"
)

type qmpExchange struct {
	execute   string
	arguments map[string]any
	responses []string
}

func startQMPSequence(t *testing.T, exchanges []qmpExchange) (string, <-chan error) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		defer close(done)
		for index, exchange := range exchanges {
			conn, err := listener.Accept()
			if err != nil {
				done <- fmt.Errorf("exchange %d accept: %w", index, err)
				return
			}
			reader := bufio.NewReader(conn)
			if _, err := fmt.Fprintln(conn, `{"QMP":{"version":{}}}`); err != nil {
				_ = conn.Close()
				done <- err
				return
			}
			if _, err := reader.ReadString('\n'); err != nil {
				_ = conn.Close()
				done <- err
				return
			}
			if _, err := fmt.Fprintln(conn, `{"event":"CAPABILITY_EVENT"}`); err != nil {
				_ = conn.Close()
				done <- err
				return
			}
			if _, err := fmt.Fprintln(conn, `{"return":{}}`); err != nil {
				_ = conn.Close()
				done <- err
				return
			}
			commandJSON, err := reader.ReadString('\n')
			if err != nil {
				_ = conn.Close()
				done <- err
				return
			}
			var command map[string]any
			if err := json.Unmarshal([]byte(commandJSON), &command); err != nil {
				_ = conn.Close()
				done <- err
				return
			}
			if command["execute"] != exchange.execute {
				_ = conn.Close()
				done <- fmt.Errorf("exchange %d execute=%v, want %s", index, command["execute"], exchange.execute)
				return
			}
			gotArguments, present := command["arguments"]
			if exchange.arguments == nil && present {
				_ = conn.Close()
				done <- fmt.Errorf("exchange %d has unexpected arguments %#v", index, gotArguments)
				return
			}
			if exchange.arguments != nil {
				gotJSON, _ := json.Marshal(gotArguments)
				wantJSON, _ := json.Marshal(exchange.arguments)
				if !present || string(gotJSON) != string(wantJSON) {
					_ = conn.Close()
					done <- fmt.Errorf("exchange %d arguments=%s, want %s", index, gotJSON, wantJSON)
					return
				}
			}
			for _, response := range exchange.responses {
				if _, err := fmt.Fprintln(conn, response); err != nil {
					_ = conn.Close()
					done <- err
					return
				}
			}
			_ = conn.Close()
		}
		done <- nil
	}()
	return socketPath, done
}

func TestIncrementalMemoryQMPCommands(t *testing.T) {
	exchanges := []qmpExchange{
		{execute: "query-status", responses: []string{`{"return":{"status":"paused"}}`}},
		{execute: "migrate", arguments: map[string]any{"uri": "file:/capture/root,memory=external"}, responses: []string{`{"event":"STOP"}`, `{"return":{}}`}},
		{execute: "query-migrate", responses: []string{`{"return":{"status":"completed"}}`}},
		{execute: "query-mem-mappings", responses: []string{`{"return":{"mappings":[{"base-host-virt-addr":8192,"size":4096,"offset":0,"page-size":4096}]}}`}},
		{execute: "query-mem-page-state", responses: []string{`{"return":{"resident":[1],"empty":[2],"page-size":4096}}`}},
		{execute: "query-mem-dirty-bitmap", responses: []string{`{"return":{"bitmap":[9223372036854775809],"page-size":4096}}`}},
	}
	socketPath, done := startQMPSequence(t, exchanges)
	client := NewStratovirtClient(1, socketPath, "/unused")
	if err := client.CreateExternalMemorySnapshot("/capture/root"); err != nil {
		t.Fatal(err)
	}
	mappings, err := client.QueryMemoryMappings()
	if err != nil || len(mappings) != 1 || mappings[0].BaseHostVirtualAddress != 8192 {
		t.Fatalf("QueryMemoryMappings() = %#v, %v", mappings, err)
	}
	pageState, err := client.QueryMemoryPageState()
	if err != nil || pageState.PageSize != 4096 || len(pageState.Resident) != 1 || len(pageState.Empty) != 1 {
		t.Fatalf("QueryMemoryPageState() = %#v, %v", pageState, err)
	}
	dirty, err := client.QueryMemoryDirtyBitmap()
	if err != nil || len(dirty.Bitmap) != 1 || dirty.Bitmap[0] != uint64(1)<<63|1 {
		t.Fatalf("QueryMemoryDirtyBitmap() = %#v, %v", dirty, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCreateExternalMemorySnapshotRequiresPausedVM(t *testing.T) {
	socketPath, done := startQMPSequence(t, []qmpExchange{
		{execute: "query-status", responses: []string{`{"return":{"status":"running"}}`}},
	})
	client := NewStratovirtClient(1, socketPath, "/unused")
	err := client.CreateExternalMemorySnapshot("/capture/root")
	if err == nil || !strings.Contains(err.Error(), `requires a paused vm, got status "running"`) {
		t.Fatalf("CreateExternalMemorySnapshot() error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPauseVMReturnsErrorWhenVMDoesNotPause(t *testing.T) {
	exchanges := []qmpExchange{
		{execute: "query-status", responses: []string{`{"return":{"status":"running"}}`}},
		{execute: "stop", responses: []string{`{"return":{}}`}},
	}
	for i := 0; i < 20; i++ {
		exchanges = append(exchanges, qmpExchange{
			execute:   "query-status",
			responses: []string{`{"return":{"status":"running"}}`},
		})
	}
	socketPath, done := startQMPSequence(t, exchanges)
	client := NewStratovirtClient(1, socketPath, "/unused")
	err := client.PauseVM()
	if err == nil || !strings.Contains(err.Error(), `timeout waiting for vm to pause: last status "running"`) {
		t.Fatalf("PauseVM() error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStratovirtBuildStartCmd(t *testing.T) {
	configuredDir := t.TempDir()
	binPath := filepath.Join(configuredDir, "stratovirt")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	pathDir := t.TempDir()
	pathBinary := filepath.Join(pathDir, "stratovirt")
	if err := os.WriteFile(pathBinary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() PATH binary error = %v", err)
	}
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	client := NewStratovirtClient(1, "/tmp/conch-qmp.sock", binPath)
	script, err := client.BuildStartCmd(&driver.ResourceArgs{
		CPUBoot:    2,
		CPUMax:     4,
		MemorySize: 1024,
		MemoryPath: "/must/not/be/used/mem.img",
		NetNSPath:  "/run/conch/netns/slot-2",
		TapName:    "tap0",
		KernelPath: "/tmp/kernel",
		InitrdPath: "/tmp/initrd",
		VsockCID:   42,
		SandboxId:  "sandbox-test",
	}, false)
	if err != nil {
		t.Fatalf("BuildStartCmd() error = %v", err)
	}

	for _, want := range []string{
		"nsenter --net=/run/conch/netns/slot-2 --",
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
	if strings.Contains(script, pathBinary) {
		t.Fatalf("script used PATH binary %q instead of configured binary:\n%s", pathBinary, script)
	}
	for _, unwanted := range []string{"/must/not/be/used/mem.img", "-incoming", "memory-backend-file"} {
		if strings.Contains(script, unwanted) {
			t.Fatalf("cold script unexpectedly contains %q:\n%s", unwanted, script)
		}
	}
}

func TestStratovirtBuildRestoreCmdUsesMappedCheckpoint(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "stratovirt")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	client := NewStratovirtClient(1, "/tmp/conch-qmp.sock", binPath)
	script, err := client.BuildStartCmd(&driver.ResourceArgs{
		CPUBoot:      1,
		CPUMax:       1,
		MemorySize:   256,
		NetNSPath:    "/run/conch/netns/slot-2",
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
		t.Fatalf("restore script missing %q:\n%s", want, script)
	}
	if !strings.Contains(script, "-m 256M") {
		t.Fatalf("restore script is missing captured memory size:\n%s", script)
	}
	if strings.Contains(script, "/must/not/be/used/mem.img") {
		t.Fatalf("restore script consumed MemoryPath:\n%s", script)
	}
}

func TestStratovirtBuildIncrementalRestoreCmdUsesInheritedMemfd(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "stratovirt")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	client := NewStratovirtClient(1, "/tmp/conch-qmp.sock", binPath)
	script, err := client.BuildStartCmd(&driver.ResourceArgs{
		CPUBoot:      2,
		CPUMax:       2,
		MemorySize:   1024,
		MemoryFormat: "incremental-v1",
		IncrementalRestore: &driver.IncrementalRestoreArgs{
			MemoryFD:       3,
			UFFDSocketPath: "/run/conch/u-one-shot.sock",
		},
		NetNSPath:    "/run/conch/netns/slot-2",
		TapName:      "tap0",
		KernelPath:   "/tmp/kernel",
		InitrdPath:   "/tmp/initrd",
		SnapfilePath: "/mnt/memory",
		VsockCID:     42,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"-S",
		"-object memory-backend-memfd,size=1024M,id=mem0,fd=3,share=off,mem-prealloc=false",
		"-numa node,nodeid=0,cpus=0-1,memdev=mem0",
		"-incoming file:/mnt/memory,memory=external,mapped=false,uffd_sock=/run/conch/u-one-shot.sock",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("incremental restore missing %q:\n%s", want, script)
		}
	}
}

func TestStratovirtBuildStartCmdRequiresNSenter(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "stratovirt")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv("PATH", binDir)

	client := NewStratovirtClient(1, "/tmp/conch-qmp.sock", binPath)
	_, err := client.BuildStartCmd(&driver.ResourceArgs{}, false)
	if err == nil || !strings.Contains(err.Error(), "resolve nsenter binary") {
		t.Fatalf("BuildStartCmd() error = %v, want missing nsenter error", err)
	}
}

func TestStratovirtPrepareLaunchDoesNotConsumeCLHSnapshotConfig(t *testing.T) {
	client := NewStratovirtClient(1, filepath.Join(t.TempDir(), "qmp.sock"), "/opt/vmm/stratovirt")
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
	if !strings.Contains(got, ",iothread=pmemio") {
		t.Fatalf("pmem device missing iothread option: %q", got)
	}
}

func TestBuildStratovirtPmemDevicesSharesSingleIothread(t *testing.T) {
	dir := t.TempDir()
	var pmemPaths []string
	for _, name := range []string{"layer0.erofs", "layer1.erofs"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, make([]byte, 2*1024*1024), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		pmemPaths = append(pmemPaths, path)
	}

	got := buildStratovirtPmemDevices(pmemPaths)
	if count := strings.Count(got, "-object iothread,id=pmemio"); count != 1 {
		t.Fatalf("iothread object count = %d, want 1 in %q", count, got)
	}
	if !strings.HasPrefix(got, "-object iothread,id=pmemio \\\n") {
		t.Fatalf("iothread object must precede the devices referencing it: %q", got)
	}
	if count := strings.Count(got, ",iothread=pmemio"); count != len(pmemPaths) {
		t.Fatalf("pmem devices carrying iothread = %d, want %d in %q", count, len(pmemPaths), got)
	}
}

func TestBuildStratovirtPmemDevicesWithoutUsablePathsIsEmpty(t *testing.T) {
	if got := buildStratovirtPmemDevices(nil); got != "" {
		t.Fatalf("buildStratovirtPmemDevices(nil) = %q, want empty", got)
	}
	// A missing file has no size, so the iothread object must not be emitted alone.
	if got := buildStratovirtPmemDevices([]string{filepath.Join(t.TempDir(), "absent.erofs")}); got != "" {
		t.Fatalf("buildStratovirtPmemDevices(missing) = %q, want empty", got)
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
