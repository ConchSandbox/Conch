package cmd

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func resolve(t *testing.T, argvPassword string, stdin bool, in io.Reader) (string, string, error) {
	t.Helper()
	var warn bytes.Buffer
	password, err := resolveRegistryPassword(argvPassword, stdin, in, &warn)
	return password, warn.String(), err
}

// Replaces os.Stdin so the paths that read it can be driven end to end.
func withStdin(t *testing.T, content string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	previous := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = previous
		_ = reader.Close()
	})
	go func() {
		_, _ = io.WriteString(writer, content)
		_ = writer.Close()
	}()
}

// The warning must name the alternative; "this is insecure" alone leaves the
// operator with nothing to do.
func TestResolveRegistryPasswordWarnsAboutArgv(t *testing.T) {
	password, warning, err := resolve(t, "secret", false, nil)
	if err != nil {
		t.Fatalf("resolveRegistryPassword: %v", err)
	}
	if password != "secret" {
		t.Fatalf("password = %q, want %q", password, "secret")
	}
	for _, want := range []string{"--password-stdin", "ps"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning does not mention %q: %s", want, warning)
		}
	}
}

func TestResolveRegistryPasswordFromStdinDoesNotWarn(t *testing.T) {
	for _, input := range []string{"secret", "secret\n", "secret\r\n"} {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(input, "\n", "\\n"), "\r", "\\r"), func(t *testing.T) {
			password, warning, err := resolve(t, "", true, strings.NewReader(input))
			if err != nil {
				t.Fatalf("resolveRegistryPassword: %v", err)
			}
			if password != "secret" {
				t.Fatalf("password = %q, want %q", password, "secret")
			}
			if warning != "" {
				t.Fatalf("stdin path warned: %s", warning)
			}
		})
	}
}

// Trimming spaces would authenticate with something else, surfacing as an
// unexplained rejection from the registry.
func TestResolveRegistryPasswordKeepsSurroundingSpaces(t *testing.T) {
	password, _, err := resolve(t, "", true, strings.NewReader("  se cret  \n"))
	if err != nil {
		t.Fatalf("resolveRegistryPassword: %v", err)
	}
	if password != "  se cret  " {
		t.Fatalf("password = %q, want the surrounding spaces preserved", password)
	}
}

// A silent preference would leave the caller believing a credential was used
// that was not.
func TestResolveRegistryPasswordRejectsConflictingSources(t *testing.T) {
	_, _, err := resolve(t, "secret", true, strings.NewReader("other\n"))
	if err == nil {
		t.Fatal("--password-stdin alongside a command-line password was accepted, want an error")
	}
	if !strings.Contains(err.Error(), "--password-stdin") {
		t.Errorf("error does not name the conflict: %v", err)
	}
}

func TestResolveRegistryPasswordStdinLimits(t *testing.T) {
	atLimit := strings.Repeat("a", maxStdinPasswordBytes)
	password, _, err := resolve(t, "", true, strings.NewReader(atLimit))
	if err != nil {
		t.Fatalf("a password exactly at the limit was rejected: %v", err)
	}
	if password != atLimit {
		t.Fatalf("password was truncated: got %d bytes, want %d", len(password), len(atLimit))
	}

	overLimit := strings.Repeat("a", maxStdinPasswordBytes+1)
	if _, _, err := resolve(t, "", true, strings.NewReader(overLimit)); err == nil {
		t.Fatal("an oversized stdin password was accepted, want an error")
	}
}

func TestResolveRegistryPasswordRejectsEmptyStdin(t *testing.T) {
	if _, _, err := resolve(t, "", true, strings.NewReader("\n")); err == nil {
		t.Fatal("an empty stdin password was accepted, want an error")
	}
}

func TestResolveRegistryPasswordEmptyWhenUnset(t *testing.T) {
	password, warning, err := resolve(t, "", false, nil)
	if err != nil || password != "" {
		t.Fatalf("resolveRegistryPassword() = %q, %v; want \"\", nil", password, err)
	}
	if warning != "" {
		t.Fatalf("an anonymous pull warned: %s", warning)
	}
}

func TestRegistryCredentials(t *testing.T) {
	tests := []struct {
		name         string
		user         string
		username     string
		argvPassword string
		wantUser     string
		wantPassword string
		wantErr      bool
	}{
		{name: "user with password", user: "alice:secret", wantUser: "alice", wantPassword: "secret"},
		{name: "separate flags", username: "alice", argvPassword: "secret", wantUser: "alice", wantPassword: "secret"},
		{name: "bare user with no password source", user: "alice", wantErr: true},
		{name: "username flag with no password source", username: "alice", wantErr: true},
		{name: "user and password together", user: "alice:secret", argvPassword: "other", wantErr: true},
		{name: "malformed user", user: "alice:", wantErr: true},
		{name: "anonymous", wantUser: "", wantPassword: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			username, password, err := registryCredentials(tt.user, tt.username, tt.argvPassword, false)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("registryCredentials() = %q, %q, nil; want an error", username, password)
				}
				return
			}
			if err != nil {
				t.Fatalf("registryCredentials: %v", err)
			}
			if username != tt.wantUser || password != tt.wantPassword {
				t.Fatalf("registryCredentials() = %q, %q; want %q, %q", username, password, tt.wantUser, tt.wantPassword)
			}
		})
	}
}

// Makes --password-stdin usable for image pull, whose only credential flag is
// --user; without a bare username the password would have to go in argv.
func TestRegistryCredentialsBareUserWithStdin(t *testing.T) {
	withStdin(t, "from-stdin\n")

	username, password, err := registryCredentials("alice", "", "", true)
	if err != nil {
		t.Fatalf("registryCredentials: %v", err)
	}
	if username != "alice" || password != "from-stdin" {
		t.Fatalf("registryCredentials() = %q, %q; want %q, %q", username, password, "alice", "from-stdin")
	}
}

// The template commands must reach the shared resolver, or the warning and
// conflict rules drift per command.
func TestTemplateRegistryCredentialsSharesTheResolver(t *testing.T) {
	withStdin(t, "from-stdin\n")

	username, password, err := templateRegistryCredentials(templateRegistryOptions{
		user:          "alice",
		passwordStdin: true,
	})
	if err != nil {
		t.Fatalf("templateRegistryCredentials: %v", err)
	}
	if username != "alice" || password != "from-stdin" {
		t.Fatalf("templateRegistryCredentials() = %q, %q; want %q, %q", username, password, "alice", "from-stdin")
	}

	if _, _, err := templateRegistryCredentials(templateRegistryOptions{
		user:     "alice:secret",
		password: "other",
	}); err == nil {
		t.Fatal("template credentials accepted two password sources, want an error")
	}
}

// image push parses flags by hand, so the new flag needs its own check.
func TestImagePushParsesPasswordStdin(t *testing.T) {
	opts, err := ParseImagePushArgs([]string{"--username", "alice", "--password-stdin", "local:tag", "remote:tag"})
	if err != nil {
		t.Fatalf("ParseImagePushArgs: %v", err)
	}
	if !opts.PasswordStdin || opts.Username != "alice" || opts.Password != "" {
		t.Fatalf("opts = %+v, want PasswordStdin with no argv password", opts)
	}
}
