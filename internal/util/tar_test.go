package util

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTarDirectoryAndUntarRoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "dir", "file.txt"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}

	archivePath := filepath.Join(t.TempDir(), "archive.tar")
	if err := TarDirectory(src, archivePath); err != nil {
		t.Fatalf("TarDirectory: %v", err)
	}

	dst := t.TempDir()
	if err := Untar(archivePath, dst); err != nil {
		t.Fatalf("Untar: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "dir", "file.txt"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("extracted content = %q", got)
	}
}

func TestUntarRejectsUnsafePath(t *testing.T) {
	archivePath := writeTarWithEntries(t, map[string]byte{"../escape": tar.TypeReg})
	err := Untar(archivePath, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsafe tar path") {
		t.Fatalf("Untar error = %v, want unsafe tar path", err)
	}
}

func TestUntarRejectsUnsupportedEntryType(t *testing.T) {
	archivePath := writeTarWithEntries(t, map[string]byte{"link": tar.TypeSymlink})
	err := Untar(archivePath, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsupported tar entry") {
		t.Fatalf("Untar error = %v, want unsupported tar entry", err)
	}
}

func writeTarWithEntries(t *testing.T, entries map[string]byte) string {
	t.Helper()
	archivePath := filepath.Join(t.TempDir(), "archive.tar")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(file)
	for name, typ := range entries {
		header := &tar.Header{
			Name:     name,
			Typeflag: byte(typ),
			Mode:     0o644,
		}
		if typ == tar.TypeReg {
			header.Size = int64(len("payload"))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte("payload")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return archivePath
}
