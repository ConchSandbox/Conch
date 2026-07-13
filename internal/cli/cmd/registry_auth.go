package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

var promptRegistryCredentials = defaultPromptRegistryCredentials

func pushWithRegistryAuth(ctx context.Context, remoteImage, username, password string, push func(string, string) error) error {
	err := push(username, password)
	if err == nil || password != "" || !isRegistryAuthError(err) {
		return err
	}

	username, password, promptErr := promptRegistryCredentials(ctx, remoteImage, username)
	if promptErr != nil {
		return fmt.Errorf("registry authentication required for %s: %w", remoteImage, promptErr)
	}
	return push(username, password)
}

func isRegistryAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"status 401",
		"status 403",
		"unauthorized",
		"authentication required",
		"authorization failed",
		"no basic auth credentials",
		"access denied",
		"requested access to the resource is denied",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func defaultPromptRegistryCredentials(ctx context.Context, remoteImage, username string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if !isTerminal(os.Stdin.Fd()) {
		return "", "", fmt.Errorf("stdin is not a terminal; rerun from a terminal or pass --username/--password for automation")
	}

	fmt.Fprintf(os.Stderr, "Registry authentication required for %s\n", remoteImage)
	username = strings.TrimSpace(username)
	if username == "" {
		fmt.Fprint(os.Stderr, "Username: ")
		input, err := readTerminalLine(ctx, os.Stdin.Fd())
		if err != nil && input == "" {
			return "", "", err
		}
		username = strings.TrimSpace(input)
	} else {
		fmt.Fprintf(os.Stderr, "Username: %s\n", username)
	}
	if username == "" {
		return "", "", fmt.Errorf("username is required")
	}

	fmt.Fprint(os.Stderr, "Password: ")
	password, err := readPasswordNoEcho(ctx, os.Stdin.Fd())
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", "", err
	}
	password = strings.TrimRight(password, "\r\n")
	if password == "" {
		return "", "", fmt.Errorf("password is required")
	}
	return username, password, nil
}

func readPasswordNoEcho(ctx context.Context, fd uintptr) (string, error) {
	oldState, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	if err != nil {
		return "", err
	}
	newState := *oldState
	newState.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(int(fd), unix.TCSETS, &newState); err != nil {
		return "", err
	}
	defer func() {
		_ = unix.IoctlSetTermios(int(fd), unix.TCSETS, oldState)
	}()
	return readTerminalLine(ctx, fd)
}

func readTerminalLine(ctx context.Context, fd uintptr) (string, error) {
	pollFDs := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	line := make([]byte, 0, 64)
	buffer := make([]byte, 256)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		ready, err := unix.Poll(pollFDs, 100)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return "", err
		}
		if ready == 0 {
			continue
		}
		revents := pollFDs[0].Revents
		if revents&unix.POLLIN == 0 {
			if revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				return "", io.EOF
			}
			continue
		}
		n, err := unix.Read(int(fd), buffer)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", io.EOF
		}
		for _, value := range buffer[:n] {
			line = append(line, value)
			if value == '\n' {
				return string(line), nil
			}
		}
	}
}
