package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Stops a mistyped `< bigfile` from being read into memory in full.
const maxStdinPasswordBytes = 64 << 10

// /proc/<pid>/cmdline is 0444 and ps prints it, so an argv password is readable
// by every local user for as long as the command runs. A pipe leaks nothing.
const argvPasswordWarning = "WARNING! Using a password on the command line is insecure: " +
	"ps and /proc/<pid>/cmdline expose it to every local user. Use --password-stdin."

var errNoPasswordSource = errors.New(
	"a registry username was given without a password; use --password-stdin or --user <name>:<password>")

func conflictingSources(a, b string) error {
	return fmt.Errorf("conflicting options: cannot specify both %s and %s", a, b)
}

// resolveRegistryPassword prefers the source that keeps the password out of
// argv. in and warn default to os.Stdin and os.Stderr.
func resolveRegistryPassword(argvPassword string, stdin bool, in io.Reader, warn io.Writer) (string, error) {
	switch {
	case stdin && argvPassword != "":
		return "", conflictingSources("--password-stdin", "a command-line password")
	case stdin:
		if in == nil {
			in = os.Stdin
		}
		return readPasswordFromStdin(in)
	case argvPassword != "":
		if warn == nil {
			warn = os.Stderr
		}
		fmt.Fprintln(warn, argvPasswordWarning)
		return argvPassword, nil
	default:
		return "", nil
	}
}

func readPasswordFromStdin(in io.Reader) (string, error) {
	// One byte over, to tell "at the limit" from "truncated".
	data, err := io.ReadAll(io.LimitReader(in, maxStdinPasswordBytes+1))
	if err != nil {
		return "", fmt.Errorf("read password from stdin: %w", err)
	}
	if len(data) > maxStdinPasswordBytes {
		return "", fmt.Errorf("password on stdin exceeds %d bytes", maxStdinPasswordBytes)
	}
	// Line terminator only: a password may legitimately end with a space.
	password := strings.TrimRight(string(data), "\r\n")
	if password == "" {
		return "", errors.New("no password was read from stdin")
	}
	return password, nil
}

// registryCredentials resolves every command through one place, so the rules
// cannot drift and a new command cannot forget the warning. user is --user,
// either "name:password" or a bare "name".
func registryCredentials(user, username, argvPassword string, stdin bool) (string, string, error) {
	if user != "" {
		if strings.ContainsRune(user, ':') {
			parsedUser, parsedPassword, err := ParseRegistryUser(user)
			if err != nil {
				return "", "", err
			}
			if argvPassword != "" {
				return "", "", conflictingSources("--user <name>:<password>", "--password")
			}
			username, argvPassword = parsedUser, parsedPassword
		} else {
			// Bare username pairs with --password-stdin; without it, naming a
			// user would force the password into argv.
			username = user
		}
	}

	password, err := resolveRegistryPassword(argvPassword, stdin, nil, nil)
	if err != nil {
		return "", "", err
	}
	if username != "" && password == "" {
		return "", "", errNoPasswordSource
	}
	return username, password, nil
}
