package cow

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	for index, argument := range os.Args {
		if argument == "--socket" && index+1 < len(os.Args) {
			os.Exit(runManagedCowTestProcess(os.Args[index+1]))
		}
	}
	os.Exit(m.Run())
}

func runManagedCowTestProcess(socketPath string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	server := newServer(socketPath)
	if err := server.Serve(ctx); err != nil {
		return 1
	}
	if err := server.Close(); err != nil {
		return 1
	}
	return 0
}

func TestStartProcessWaitsForPingAndClosesChild(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "cow.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	process, err := StartProcess(ctx, os.Args[0], socketPath)
	if err != nil {
		t.Fatal(err)
	}
	pid := process.cmd.Process.Pid
	t.Cleanup(func() {
		if process.cmd.ProcessState == nil {
			_ = process.cmd.Process.Kill()
			_, _ = process.cmd.Process.Wait()
		}
	})

	if err := NewClient(socketPath).Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	if process.cmd.ProcessState == nil || !process.cmd.ProcessState.Exited() {
		t.Fatalf("cow process state = %#v, want exited", process.cmd.ProcessState)
	}
	if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("cow pid %d remains after Close: %v", pid, err)
	}
}

func TestStartProcessFailsWhenChildExitsBeforeReady(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "cow.sock")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	process, err := StartProcess(ctx, "/bin/false", socketPath)
	if process != nil || err == nil || !strings.Contains(err.Error(), "before becoming ready") {
		t.Fatalf("StartProcess() process=%v error=%v", process, err)
	}
}
