package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	unixSocketMode = 0o660
	// bind(2) creates the socket at 0o777 &^ umask, and 0o777 &^ 0o117 == 0o660,
	// so it lands at its final mode with no chmod to race against.
	unixSocketBindUmask = 0o117
	// Applied only to directories conchd creates itself; see prepareUnixSocketDir.
	unixSocketDirMode            = 0o750
	recommendedUnixSocketDirMode = 0o750
)

// listenUnixSocket binds the control-plane socket, never wider than its final
// mode.
func listenUnixSocket(path string) (net.Listener, error) {
	if err := prepareUnixSocketDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove stale unix socket: %w", err)
	}

	// Process-global, so restore it as soon as bind(2) returns.
	previousUmask := unix.Umask(unixSocketBindUmask)
	ln, err := net.Listen("unix", path)
	unix.Umask(previousUmask)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on unix socket %s: %w", path, err)
	}

	if err := verifyUnixSocketMode(path); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return ln, nil
}

// verifyUnixSocketMode fails closed: an unexpected mode means the boundary is
// not the one conchd asked for.
func verifyUnixSocketMode(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("failed to inspect unix socket %s: %w", path, err)
	}
	if mode := info.Mode().Perm(); mode != unixSocketMode {
		return fmt.Errorf(
			"unix socket %s has unexpected permissions %04o, want %04o",
			path, mode, unixSocketMode,
		)
	}
	return nil
}

// prepareUnixSocketDir creates a missing directory restricted and only warns
// about an existing one: server.unix_socket is operator-configurable, so
// filepath.Dir can be /run, and narrowing that would break the host.
func prepareUnixSocketDir(dir string) error {
	info, err := os.Stat(dir)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("unix socket directory %s is not a directory", dir)
		}
		if mode := info.Mode().Perm(); mode&^recommendedUnixSocketDirMode != 0 {
			ulog.GetLogger().Warn("Unix socket directory is more permissive than recommended",
				ulog.F("dir", dir),
				ulog.F("mode", fmt.Sprintf("%04o", mode)),
				ulog.F("recommended", fmt.Sprintf("%04o", recommendedUnixSocketDirMode)),
			)
		}
		return nil
	case os.IsNotExist(err):
		if err := os.MkdirAll(dir, unixSocketDirMode); err != nil {
			return fmt.Errorf("failed to create unix socket directory: %w", err)
		}
		// MkdirAll applies the ambient umask, so restate the mode.
		if err := os.Chmod(dir, unixSocketDirMode); err != nil {
			return fmt.Errorf("failed to set unix socket directory permissions: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("failed to inspect unix socket directory %s: %w", dir, err)
	}
}
