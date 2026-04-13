package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitDockerfileAndContext_DefaultDockerfile(t *testing.T) {
	ctxDir := t.TempDir()
	dockerfile := filepath.Join(ctxDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	gotDockerfile, gotContext, gotRest, err := splitDockerfileAndContext([]string{"-t", "demo:latest", ctxDir})
	if err != nil {
		t.Fatalf("splitDockerfileAndContext: %v", err)
	}
	if gotDockerfile != dockerfile {
		t.Fatalf("dockerfile: got %q want %q", gotDockerfile, dockerfile)
	}
	if gotContext != ctxDir {
		t.Fatalf("context: got %q want %q", gotContext, ctxDir)
	}
	if len(gotRest) != 2 || gotRest[0] != "-t" || gotRest[1] != "demo:latest" {
		t.Fatalf("unexpected rest args: %#v", gotRest)
	}
}

func TestSplitDockerfileAndContext_RejectsRemoteContext(t *testing.T) {
	_, _, _, err := splitDockerfileAndContext([]string{"-f", "Dockerfile", "https://example.com/demo.git"})
	if err == nil {
		t.Fatal("expected remote context to fail")
	}
	if !strings.Contains(err.Error(), "remote build context") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSplitDockerfileAndContext_DoesNotTreatBuildArgAsContext(t *testing.T) {
	ctxDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(ctxDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})

	dockerfile := filepath.Join(ctxDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}

	gotDockerfile, gotContext, gotRest, err := splitDockerfileAndContext([]string{"--build-arg", "FOO=bar"})
	if err != nil {
		t.Fatalf("splitDockerfileAndContext: %v", err)
	}
	if gotDockerfile != "Dockerfile" {
		t.Fatalf("dockerfile: got %q want %q", gotDockerfile, "Dockerfile")
	}
	if gotContext != "." {
		t.Fatalf("context: got %q want %q", gotContext, ".")
	}
	if len(gotRest) != 2 || gotRest[0] != "--build-arg" || gotRest[1] != "FOO=bar" {
		t.Fatalf("unexpected rest args: %#v", gotRest)
	}
}

func TestBuildahArgsPlaceContextLast(t *testing.T) {
	patched := patchDockerfileFlag([]string{"-t", "demo:latest"}, "/tmp/processed.Dockerfile")
	patched = ensureBuildahIsolation(patched)
	patched = append(patched, "--iidfile", "/tmp/imageid")
	patched = append(patched, "/tmp/context")

	if got, want := patched[len(patched)-1], "/tmp/context"; got != want {
		t.Fatalf("context must be last arg: got %q want %q (args=%#v)", got, want, patched)
	}

	for i := 0; i < len(patched)-1; i++ {
		if patched[i] == "/tmp/context" {
			t.Fatalf("context appeared before end of args: %#v", patched)
		}
	}

	foundIID := false
	for i := 0; i < len(patched)-1; i++ {
		if patched[i] == "--iidfile" && i+1 < len(patched)-1 && patched[i+1] == "/tmp/imageid" {
			foundIID = true
			break
		}
	}
	if !foundIID {
		t.Fatalf("missing --iidfile pair before context: %#v", patched)
	}
}

func TestEnsureBuildahIsolationAddsDefault(t *testing.T) {
	got := ensureBuildahIsolation([]string{"-t", "demo:latest"})
	want := []string{"--isolation", "chroot", "-t", "demo:latest"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ensureBuildahIsolation() = %#v, want %#v", got, want)
	}
}

func TestEnsureBuildahIsolationRespectsExistingValue(t *testing.T) {
	tests := [][]string{
		{"--isolation", "oci", "-t", "demo:latest"},
		{"--isolation=oci", "-t", "demo:latest"},
	}

	for _, tt := range tests {
		got := ensureBuildahIsolation(tt)
		if strings.Join(got, "\x00") != strings.Join(tt, "\x00") {
			t.Fatalf("ensureBuildahIsolation(%#v) = %#v, want unchanged", tt, got)
		}
	}
}

func TestFirstImageTag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "short", args: []string{"-t", "demo:latest", "."}, want: "demo:latest"},
		{name: "long", args: []string{"--tag", "demo:latest", "."}, want: "demo:latest"},
		{name: "equals", args: []string{"--tag=demo:latest", "."}, want: "demo:latest"},
		{name: "missing", args: []string{"."}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstImageTag(tt.args); got != tt.want {
				t.Fatalf("firstImageTag(%#v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
