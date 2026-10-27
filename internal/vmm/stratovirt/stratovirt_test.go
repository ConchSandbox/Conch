package stratovirt

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/vmm/driver"
)

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
		"ipv6.disable=1",
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

// bind creates the socket file before listen makes it dialable: the wait must
// span that gap, exactly like StratoVirt startup.
func TestWaitForVmmSocketWaitsUntilDialable(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket() error = %v", err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: socketPath}); err != nil {
		t.Fatalf("bind() error = %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		if err := syscall.Listen(fd, 16); err != nil {
			t.Errorf("listen() error = %v", err)
		}
	}()

	if err := waitForVmmSocket(ctx, socketPath, nil); err != nil {
		t.Fatalf("waitForVmmSocket() error = %v", err)
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("socket %s not dialable after wait: %v", socketPath, err)
	}
	conn.Close()
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

// A QMP server that accepts but never sends its greeting must fail on the
// connection deadline, not hang the caller.
func TestConnectQMPTimesOutOnStalledGreeting(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen() error = %v", err)
	}
	defer listener.Close()
	held := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			held <- conn
		}
	}()
	t.Cleanup(func() {
		select {
		case conn := <-held:
			conn.Close()
		default:
		}
	})
	shortenQMPTimeout(t, 200*time.Millisecond)

	client := NewStratovirtClient(1, socketPath, "/opt/vmm/stratovirt")
	_, _, err = client.connectQMP()
	if err == nil {
		t.Fatalf("connectQMP() succeeded against a stalled qmp server")
	}
	if !strings.Contains(err.Error(), "failed to read qmp greeting") {
		t.Fatalf("connectQMP() error = %q, want greeting read failure", err.Error())
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("connectQMP() error = %v, want deadline exceeded", err)
	}
}

// A QMP server that answers the greeting but stalls the capabilities response
// fails on the same connection deadline.
func TestConnectQMPTimesOutOnStalledCapabilities(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen() error = %v", err)
	}
	defer listener.Close()
	held := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		if _, err := conn.Write([]byte(`{"QMP":{"version":{}}}` + "\n")); err != nil {
			conn.Close()
			return
		}
		held <- conn
	}()
	t.Cleanup(func() {
		select {
		case conn := <-held:
			conn.Close()
		default:
		}
	})
	shortenQMPTimeout(t, 200*time.Millisecond)

	client := NewStratovirtClient(1, socketPath, "/opt/vmm/stratovirt")
	_, _, err = client.connectQMP()
	if err == nil {
		t.Fatalf("connectQMP() succeeded against a stalled capabilities response")
	}
	if !strings.Contains(err.Error(), "failed to read qmp_capabilities response") {
		t.Fatalf("connectQMP() error = %q, want capabilities read failure", err.Error())
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("connectQMP() error = %v, want deadline exceeded", err)
	}
}

func shortenQMPTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := qmpTimeout
	qmpTimeout = d
	t.Cleanup(func() { qmpTimeout = old })
}

// TestCreateSnapshotWaitsOutSlowMigrate pins the migrate read's own budget:
// StratoVirt answers only after writing the full VM state, so the response
// can outlive the handshake deadline. A migrate slower than qmpTimeout but
// well inside the snapshot budget must still complete.
func TestCreateSnapshotWaitsOutSlowMigrate(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen() error = %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveQMPCommand(conn)
		}
	}()
	shortenQMPTimeout(t, 200*time.Millisecond)

	client := NewStratovirtClient(1, socketPath, "/opt/vmm/stratovirt")
	snapPath := filepath.Join(t.TempDir(), "snap.vmstate")
	started := time.Now()
	if err := client.CreateSnapshot(snapPath); err != nil {
		t.Fatalf("CreateSnapshot() error = %v, want the slow migrate to complete", err)
	}
	if elapsed := time.Since(started); elapsed < 500*time.Millisecond {
		t.Fatalf("CreateSnapshot() returned in %v without waiting for the migrate response", elapsed)
	}
}

// TestPauseVMFailsWithoutConfirmation pins the poll's terminal verdict: a guest still reporting running after stop must surface as an error, never success.
func TestPauseVMFailsWithoutConfirmation(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "qmp.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen() error = %v", err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveQMPAlwaysRunning(conn)
		}
	}()

	client := NewStratovirtClient(1, socketPath, "/opt/vmm/stratovirt")
	if err := client.PauseVM(); err == nil {
		t.Fatal("PauseVM() = nil without a paused or stopped confirmation, want error")
	}
}

// serveQMPAlwaysRunning answers every command with a running status.
func serveQMPAlwaysRunning(conn net.Conn) {
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"QMP":{"version":{}}}` + "\n")); err != nil {
		return
	}
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		return
	}
	if _, err := conn.Write([]byte(`{"return":{}}` + "\n")); err != nil {
		return
	}
	for {
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		if _, err := conn.Write([]byte(`{"return":{"status":"running"}}` + "\n")); err != nil {
			return
		}
	}
}

// serveQMPCommand speaks one command per connection: the handshake, then the
// command's scripted answer. The migrate answer is delayed past a shortened
// qmpTimeout to prove the migrate read does not share that deadline.
func serveQMPCommand(conn net.Conn) {
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"QMP":{"version":{}}}` + "\n")); err != nil {
		return
	}
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		return
	}
	if _, err := conn.Write([]byte(`{"return":{}}` + "\n")); err != nil {
		return
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	var cmd struct {
		Execute string `json:"execute"`
	}
	if err := json.Unmarshal([]byte(line), &cmd); err != nil {
		return
	}
	switch cmd.Execute {
	case "migrate":
		time.Sleep(500 * time.Millisecond)
		if _, err := conn.Write([]byte(`{"return":{}}` + "\n")); err != nil {
			return
		}
	case "query-migrate":
		if _, err := conn.Write([]byte(`{"return":{"status":"completed"}}` + "\n")); err != nil {
			return
		}
	default:
		if _, err := conn.Write([]byte(`{"return":{}}` + "\n")); err != nil {
			return
		}
	}
}
