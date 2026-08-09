package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// These mutate the process-global umask, so none may call t.Parallel().

func withUmask(t *testing.T, mask int) {
	t.Helper()
	previous := unix.Umask(mask)
	t.Cleanup(func() { unix.Umask(previous) })
}

func socketPath(t *testing.T) string {
	t.Helper()
	// sun_path is capped at 108 bytes; t.TempDir() names can overflow it.
	dir, err := os.MkdirTemp("", "conchsock")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "conchd.sock")
}

// Under umask 0, bind(2) would create the socket 0777. Since nothing widens it
// afterwards, 0660 on return also proves it was never wider.
func TestListenUnixSocketIsNeverWiderThanItsFinalMode(t *testing.T) {
	withUmask(t, 0)
	path := socketPath(t)

	ln, err := listenUnixSocket(path)
	if err != nil {
		t.Fatalf("listenUnixSocket: %v", err)
	}
	defer ln.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if mode := info.Mode().Perm(); mode != unixSocketMode {
		t.Fatalf("socket mode = %04o, want %04o", mode, unixSocketMode)
	}
	if mode := info.Mode().Perm(); mode&0o007 != 0 {
		t.Fatalf("socket mode %04o grants access to other users", mode)
	}
}

func TestListenUnixSocketRestoresAmbientUmask(t *testing.T) {
	const ambient = 0o022
	withUmask(t, ambient)
	path := socketPath(t)

	ln, err := listenUnixSocket(path)
	if err != nil {
		t.Fatalf("listenUnixSocket: %v", err)
	}
	defer ln.Close()

	current := unix.Umask(ambient)
	if current != ambient {
		t.Fatalf("umask after listenUnixSocket = %04o, want %04o", current, ambient)
	}
}

func TestListenUnixSocketCreatesMissingDirectoryRestricted(t *testing.T) {
	withUmask(t, 0)
	base := socketPath(t)
	path := filepath.Join(filepath.Dir(base), "nested", "conchd.sock")

	ln, err := listenUnixSocket(path)
	if err != nil {
		t.Fatalf("listenUnixSocket: %v", err)
	}
	defer ln.Close()

	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat socket dir: %v", err)
	}
	if mode := info.Mode().Perm(); mode != unixSocketDirMode {
		t.Fatalf("socket dir mode = %04o, want %04o", mode, unixSocketDirMode)
	}
}

// Guards the decision not to chmod a directory conchd did not create:
// filepath.Dir can be a shared system directory like /run.
func TestListenUnixSocketLeavesExistingDirectoryMode(t *testing.T) {
	withUmask(t, 0)
	path := socketPath(t)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}

	ln, err := listenUnixSocket(path)
	if err != nil {
		t.Fatalf("listenUnixSocket: %v", err)
	}
	defer ln.Close()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat socket dir: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o755 {
		t.Fatalf("pre-existing socket dir mode = %04o, want it untouched at 0755", mode)
	}
}

func TestListenUnixSocketReplacesStaleSocket(t *testing.T) {
	withUmask(t, 0)
	path := socketPath(t)

	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	// Close without unlinking, as an abnormal exit would.
	if unixLn, ok := stale.(*net.UnixListener); ok {
		unixLn.SetUnlinkOnClose(false)
	}
	if err := stale.Close(); err != nil {
		t.Fatalf("close stale socket: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket should still exist: %v", err)
	}

	ln, err := listenUnixSocket(path)
	if err != nil {
		t.Fatalf("listenUnixSocket over stale socket: %v", err)
	}
	defer ln.Close()
}

// Keeps the permission assertions honest: an unreachable socket satisfies them all.
func TestListenUnixSocketAcceptsConnections(t *testing.T) {
	withUmask(t, 0)
	path := socketPath(t)

	ln, err := listenUnixSocket(path)
	if err != nil {
		t.Fatalf("listenUnixSocket: %v", err)
	}
	defer ln.Close()

	accepted := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
		accepted <- err
	}()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	_ = conn.Close()
	if err := <-accepted; err != nil {
		t.Fatalf("accept: %v", err)
	}
}

func TestVerifyUnixSocketModeRejectsWidenedMode(t *testing.T) {
	withUmask(t, 0)
	path := socketPath(t)

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := verifyUnixSocketMode(path); err == nil {
		t.Fatal("verifyUnixSocket accepted a 0666 socket, want an error")
	}
}
