package volume

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const issue083RestoredFixtureEnv = "CONCH_ISSUE_083_RESTORED_FIXTURE"

// TestIssue083RestoredVirtiofsdDeathIsObservable is intentionally compatible
// with the issue base. It uses only the pre-existing Backend contract and
// reflects for the repaired read-only health method, so the exact same source
// compiles there and fails after observing that restored-process death has no
// monitor or observable health result.
func TestIssue083RestoredVirtiofsdDeathIsObservable(t *testing.T) {
	fixture := newIssue083RestoredFixture(t)
	if err := fixture.backend.Restore(fixture.namespace, fixture.sandboxID, []Device{fixture.device}); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}

	fixture.exit(t)
	healthMethod := reflect.ValueOf(fixture.backend).MethodByName("CheckHealth")
	if !healthMethod.IsValid() {
		t.Fatal("unexpected restored virtiofsd death is not monitored or observable: backend has no CheckHealth method")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		results := healthMethod.Call([]reflect.Value{
			reflect.ValueOf(fixture.namespace),
			reflect.ValueOf(fixture.sandboxID),
		})
		if len(results) != 1 {
			t.Fatalf("CheckHealth result count = %d, want 1", len(results))
		}
		if !results[0].IsNil() {
			healthErr, ok := results[0].Interface().(error)
			if !ok {
				t.Fatalf("CheckHealth result type = %T, want error", results[0].Interface())
			}
			if !strings.Contains(healthErr.Error(), "volume backend unhealthy") {
				t.Fatalf("CheckHealth() error = %v, want unhealthy backend error", healthErr)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("unexpected restored virtiofsd death was not reflected in CheckHealth within 1s")
}

func TestIssue083RestoredFixtureProcess(t *testing.T) {
	if os.Getenv(issue083RestoredFixtureEnv) != "1" {
		return
	}
	trigger := os.Getenv(issue083RestoredFixtureEnv + "_TRIGGER")
	for {
		if _, err := os.Stat(trigger); err == nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

type issue083RestoredFixture struct {
	backend   Backend
	namespace string
	sandboxID string
	device    Device
	cmd       *exec.Cmd
	trigger   string
	listener  net.Listener
	waitOnce  sync.Once
	waitErr   error
}

func newIssue083RestoredFixture(t *testing.T) *issue083RestoredFixture {
	t.Helper()
	runtimeDir, err := os.MkdirTemp("", "conch-083-")
	if err != nil {
		t.Fatalf("create fixture runtime directory: %v", err)
	}
	namespace := "issue-083-ns"
	sandboxID := "issue-083-restored"
	sandboxDir := filepath.Join(runtimeDir, sandboxID)
	volumeDir := filepath.Join(sandboxDir, volumeDirName)
	if err := os.MkdirAll(volumeDir, 0o755); err != nil {
		t.Fatalf("create fixture volume directory: %v", err)
	}
	// Keep config outside volumeDir so Cleanup sees an empty shared directory;
	// this fixture never mounts and therefore works without host privileges.
	configPath := filepath.Join(sandboxDir, configFileName)
	if err := os.WriteFile(configPath, []byte(`{"version":1,"mounts":[]}`), 0o600); err != nil {
		t.Fatalf("write fixture config: %v", err)
	}
	socket := filepath.Join(sandboxDir, socketName)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen on fixture socket: %v", err)
	}
	trigger := filepath.Join(runtimeDir, "exit")
	cmd := exec.Command(os.Args[0], "-test.run=^TestIssue083RestoredFixtureProcess$")
	cmd.Env = append(os.Environ(),
		issue083RestoredFixtureEnv+"=1",
		issue083RestoredFixtureEnv+"_TRIGGER="+trigger,
		"GORACE=atexit_sleep_ms=0",
	)
	if err := cmd.Start(); err != nil {
		listener.Close()
		t.Fatalf("start restored-process fixture: %v", err)
	}
	startTime := waitIssue083StartTime(t, cmd.Process.Pid)
	fixture := &issue083RestoredFixture{
		backend:   NewVirtiofsBackend(VirtiofsConfig{RuntimeDir: runtimeDir}),
		namespace: namespace,
		sandboxID: sandboxID,
		device: Device{
			SandboxID:  sandboxID,
			Namespace:  namespace,
			Backend:    DefaultBackend,
			Socket:     socket,
			VolumeDir:  volumeDir,
			ConfigPath: configPath,
			PID:        cmd.Process.Pid,
			StartTime:  startTime,
		},
		cmd:      cmd,
		trigger:  trigger,
		listener: listener,
	}
	t.Cleanup(func() {
		_ = fixture.backend.Cleanup(namespace, sandboxID, []Device{fixture.device})
		_ = cmd.Process.Kill()
		fixture.reap()
		_ = listener.Close()
		_ = os.RemoveAll(runtimeDir)
	})
	return fixture
}

func (f *issue083RestoredFixture) exit(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.trigger, []byte("exit"), 0o600); err != nil {
		t.Fatalf("trigger restored fixture exit: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join("/proc", stringPathInt(f.cmd.Process.Pid), "stat"))
		if os.IsNotExist(err) {
			return
		}
		if err == nil {
			idx := strings.LastIndexByte(string(data), ')')
			if idx >= 0 {
				fields := strings.Fields(string(data)[idx+1:])
				if len(fields) > 0 && fields[0] == "Z" {
					return
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("restored fixture did not exit within 1s")
}

func (f *issue083RestoredFixture) reap() error {
	f.waitOnce.Do(func() {
		f.waitErr = f.cmd.Wait()
	})
	return f.waitErr
}

func waitIssue083StartTime(t *testing.T, pid int) uint64 {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if startTime := processStartTicks(pid); startTime != 0 {
			return startTime
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("process %d did not expose a start time", pid)
	return 0
}

func stringPathInt(value int) string {
	return fmt.Sprintf("%d", value)
}
