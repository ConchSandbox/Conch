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

func startQMPStatusServer(t *testing.T, socketPath string, statuses []string) <-chan string {
	t.Helper()
	if len(statuses) == 0 {
		t.Fatal("startQMPStatusServer requires at least one status")
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	commands := make(chan string, 8)
	go func() {
		statusIndex := 0
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			reader := bufio.NewReader(conn)
			_, _ = fmt.Fprintln(conn, `{"QMP":{"version":{}}}`)
			_, _ = reader.ReadString('\n')
			_, _ = fmt.Fprintln(conn, `{"return":{}}`)

			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				_ = conn.Close()
				continue
			}
			var request struct {
				Execute string `json:"execute"`
			}
			if err := json.Unmarshal([]byte(line), &request); err != nil {
				_ = conn.Close()
				continue
			}
			commands <- request.Execute
			if request.Execute == "query-status" {
				status := statuses[len(statuses)-1]
				if statusIndex < len(statuses) {
					status = statuses[statusIndex]
					statusIndex++
				}
				_, _ = fmt.Fprintf(conn, "{\"return\":{\"status\":%q,\"running\":%t}}\n", status, status == "running")
			} else {
				_, _ = fmt.Fprintln(conn, `{"return":{}}`)
			}
			_ = conn.Close()
		}
	}()
	return commands
}

func TestCheckAgentAliveDoesNotResumeRunningVM(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	commands := startQMPStatusServer(t, socketPath, []string{"running"})
	client := NewStratovirtClient(1, socketPath, "/opt/vmm/stratovirt")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.CheckAgentAlive(ctx, make(chan error)); err != nil {
		t.Fatalf("CheckAgentAlive() error = %v", err)
	}
	if got := <-commands; got != "query-status" {
		t.Fatalf("first QMP command = %q, want query-status", got)
	}
	select {
	case got := <-commands:
		t.Fatalf("unexpected QMP command %q", got)
	default:
	}
}

func TestCheckAgentAliveResumesPausedVMOnce(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	commands := startQMPStatusServer(t, socketPath, []string{"paused", "running"})
	client := NewStratovirtClient(1, socketPath, "/opt/vmm/stratovirt")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.CheckAgentAlive(ctx, make(chan error)); err != nil {
		t.Fatalf("CheckAgentAlive() error = %v", err)
	}
	for i, want := range []string{"query-status", "cont", "query-status"} {
		if got := <-commands; got != want {
			t.Fatalf("QMP command %d = %q, want %q", i, got, want)
		}
	}
	select {
	case got := <-commands:
		t.Fatalf("unexpected QMP command %q", got)
	default:
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
	client.logDir = t.TempDir()
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
		"ipv6.disable=1",
		"-m 1024M",
		filepath.Join(client.logDir, "sandbox-test", "vmm.log"),
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
	client.logDir = t.TempDir()
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
	processExited := &testProcessExit{done: make(chan struct{}), err: processErr}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	close(processExited.done)

	err := waitForVmmSocket(ctx, socketPath, processExited)
	if !errors.Is(err, processErr) {
		t.Fatalf("waitForVmmSocket() error = %v, want %v", err, processErr)
	}
	if !strings.Contains(err.Error(), "exited before vmm socket") {
		t.Fatalf("waitForVmmSocket() error = %q, want early exit context", err.Error())
	}
}

type testProcessExit struct {
	done chan struct{}
	err  error
}

func (p *testProcessExit) Done() <-chan struct{} { return p.done }
func (p *testProcessExit) Err() error            { return p.err }
