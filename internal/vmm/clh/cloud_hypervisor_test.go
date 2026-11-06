package clh

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/vmm/driver"
	"golang.org/x/sys/unix"
)

func TestBuildStartCmdUsesConchNetNSPath(t *testing.T) {
	configuredDir := t.TempDir()
	binPath := filepath.Join(configuredDir, "cloud-hypervisor")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	pathDir := t.TempDir()
	pathBinary := filepath.Join(pathDir, "cloud-hypervisor")
	if err := os.WriteFile(pathBinary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() PATH binary error = %v", err)
	}
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	client := NewCLHClient(1, "/tmp/conch-clh.sock", binPath)
	args := &driver.ResourceArgs{
		CPUBoot:         2,
		CPUMax:          4,
		MemorySize:      1024,
		MemoryPath:      "/tmp/memory",
		NetNSPath:       "/run/conch/netns/slot-2",
		TapName:         "tap0",
		KernelPath:      "/tmp/kernel",
		InitrdPath:      "/tmp/initrd",
		VsockCID:        42,
		VsockSocketPath: "/tmp/vsock.sock",
		SandboxId:       "sandbox-test",
		EventMonitorFd:  10,
		ApiSocketFd:     11,
	}
	script, err := client.BuildStartCmd(args, false)
	if err != nil {
		t.Fatalf("BuildStartCmd() error = %v", err)
	}

	for _, want := range []string{
		"nsenter --net=/run/conch/netns/slot-2 --",
		binPath,
		`--net "tap=tap0"`,
		"conch.sandbox_id=sandbox-test",
		"ipv6.disable=1",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, pathBinary) {
		t.Fatalf("script used PATH binary %q instead of configured binary:\n%s", pathBinary, script)
	}
	if strings.Contains(script, "--balloon") {
		t.Fatal("balloon device enabled by default")
	}
	args.EnableBalloon = true
	script, err = client.BuildStartCmd(args, false)
	if err != nil || !strings.Contains(script, "--balloon size=0") {
		t.Fatalf("balloon startup: script=%s error=%v", script, err)
	}
}

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

func TestParseEventsFromFdParsesEventStream(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair() error = %v", err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	payload := `{"timestamp":0,"source":"vm","event":"created"}` + "\n" +
		`{"timestamp":1,"source":"vm","event":"booted"}` + "\n"
	if _, err := unix.Write(fds[1], []byte(payload)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	events, err := parseEventsFromFd(fds[0], make([]byte, 4096))
	if err != nil {
		t.Fatalf("parseEventsFromFd() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}
	if events[1].Source != "vm" || events[1].Event != EventBooted {
		t.Fatalf("second event = %#v, want vm/%s", events[1], EventBooted)
	}
}

func TestWaitVmReadyFdReturnsOnBootEvent(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair() error = %v", err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go func() {
		_, _ = unix.Write(fds[1], []byte(`{"timestamp":1,"source":"vm","event":"booted"}`+"\n"))
	}()

	if err := waitVmReadyFd(ctx, fds[0], "vm", EventBooted); err != nil {
		t.Fatalf("waitVmReadyFd() error = %v", err)
	}
}

func TestWaitVmReadyFdHandlesSplitBootEvent(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Socketpair() error = %v", err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go func() {
		_, _ = unix.Write(fds[1], []byte(`{"timestamp":1,"source":"vm"`))
		time.Sleep(10 * time.Millisecond)
		_, _ = unix.Write(fds[1], []byte(`,"event":"booted"}`+"\n"))
	}()

	if err := waitVmReadyFd(ctx, fds[0], "vm", EventBooted); err != nil {
		t.Fatalf("waitVmReadyFd() error = %v", err)
	}
}

func TestCreateVmmFdsCleanupRemovesSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "vmm.sock")
	fds, err := createVmmFds(socketPath)
	if err != nil {
		t.Fatalf("createVmmFds() error = %v", err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("expected API socket path to exist: %v", err)
	}

	fds.cleanup()

	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket path still exists after cleanup, stat err = %v", err)
	}
}

func TestVmmFdsCleanupNilSafe(t *testing.T) {
	var fds *VmmFds
	fds.cleanup()
}

func TestCLHBalloonCommands(t *testing.T) {
	resizes := make(chan uint64, 2)
	socket := balloonHTTPSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/vm.resize":
			var body struct {
				Size uint64 `json:"desired_balloon"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			resizes <- body.Size
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/vm.balloon":
			_, _ = w.Write([]byte(`{"balloon_actual":1073741824}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	client := NewCLHClient(1, socket, "/unused/cloud-hypervisor")
	client.memoryMB = 4096
	for _, target := range []int64{1024, 4096} {
		if err := client.SetMemoryTarget(t.Context(), target); err != nil {
			t.Fatal(err)
		}
		if size := <-resizes; size != uint64(4096-target)<<20 {
			t.Fatalf("target %d produced balloon size %d", target, size)
		}
	}
	actual, err := client.BalloonActual(t.Context())
	if err != nil || actual != 3072<<20 {
		t.Fatalf("actual = %d, error = %v; want 3 GiB", actual, err)
	}
}

func TestCLHBalloonRejectsInvalidResponses(t *testing.T) {
	for _, body := range []string{`{}`, `{"balloon_actual":5368709120}`, "invalid JSON", "device unavailable"} {
		t.Run(body, func(t *testing.T) {
			socket := balloonHTTPSocket(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if body == "device unavailable" {
					w.WriteHeader(http.StatusInternalServerError)
				}
				_, _ = w.Write([]byte(body))
			}))
			client := NewCLHClient(1, socket, "/unused/cloud-hypervisor")
			client.memoryMB = 4096
			if _, err := client.BalloonActual(t.Context()); err == nil {
				t.Fatal("accepted an invalid balloon response")
			} else if body == "device unavailable" && !strings.Contains(err.Error(), body) {
				t.Fatalf("VMM error detail lost: %v", err)
			}
		})
	}
}

func TestCLHBalloonCommunicationTimeout(t *testing.T) {
	release := make(chan struct{})
	socket := balloonHTTPSocket(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release) })
	client := NewCLHClient(1, socket, "/unused/cloud-hypervisor")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.BalloonActual(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("balloon query returned %v after %v", err, time.Since(start))
	}
}

func balloonHTTPSocket(t *testing.T, handler http.Handler) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "balloon.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return path
}
