package guestd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/openeuler/Conch/pkg/ulog"
)

type commandPath struct {
	name       string
	candidates []string
	once       sync.Once
	warnOnce   sync.Once
	path       string
	found      bool
}

func newCommandPath(name string, candidates ...string) *commandPath {
	return &commandPath{
		name:       name,
		candidates: candidates,
	}
}

func (c *commandPath) lookup() {
	c.once.Do(func() {
		for _, candidate := range c.candidates {
			info, err := os.Stat(candidate)
			if err == nil && !info.IsDir() {
				c.path = candidate
				c.found = true
				return
			}
		}

		if path, err := exec.LookPath(c.name); err == nil {
			c.path = path
			c.found = true
			return
		}

		if len(c.candidates) > 0 {
			c.path = c.candidates[0]
		}
	})
}

func (c *commandPath) get() string {
	c.lookup()
	if !c.found && c.path != "" {
		c.warnOnce.Do(func() {
			ulog.GetLogger().Warn("Command not found, falling back to expected path",
				ulog.F("command", c.name), ulog.F("path", c.path))
		})
	}

	return c.path
}

func (c *commandPath) available() bool {
	c.lookup()
	return c.found
}

var (
	mountCommand  = newCommandPath("mount", "/bin/mount", "/usr/bin/mount", "/sbin/mount", "/usr/sbin/mount")
	chrootCommand = newCommandPath("chroot", "/usr/sbin/chroot", "/usr/bin/chroot", "/bin/chroot")
	ipCommand     = newCommandPath("ip", "/sbin/ip", "/usr/sbin/ip", "/usr/bin/ip", "/bin/ip")
)

func execMount(args ...string) *exec.Cmd {
	return exec.Command(mountCommand.get(), args...)
}

func execChroot(args ...string) *exec.Cmd {
	return exec.Command(chrootCommand.get(), args...)
}

func execIP(args ...string) *exec.Cmd {
	return exec.Command(ipCommand.get(), args...)
}

func isMountPoint(target string) bool {
	target = filepath.Clean(target)

	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}

	lines := splitLines(string(data))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 4 && fields[4] == target {
			return true
		}
	}

	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if start < i {
				lines = append(lines, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
