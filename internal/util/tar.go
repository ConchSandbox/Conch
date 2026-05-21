package util

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// TarDirectory writes srcDir into archivePath as an uncompressed tar archive.
//
// The archive contains entries relative to srcDir; srcDir itself is not emitted
// as a top-level directory. Parent directories for archivePath are created when
// needed. TarDirectory preserves regular file modes and directory modes from
// the source tree, but it intentionally stores names using slash-separated
// relative paths so the archive is stable across platforms.
//
// Only regular files and directories are expected. Other filesystem entry types
// are passed through tar.FileInfoHeader, but callers should avoid using this for
// archives that need to preserve special files, hard links, symlinks, extended
// attributes, or sparse file metadata.
func TarDirectory(srcDir, archivePath string) error {
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		return err
	}
	out, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("create archive %s: %w", archivePath, err)
	}
	defer out.Close()
	tw := tar.NewWriter(out)
	defer tw.Close()

	return filepath.WalkDir(srcDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == srcDir {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(tw, file)
		return err
	})
}

// Untar extracts an uncompressed tar archive into dstDir.
//
// Extraction is intentionally strict: entries must be relative paths inside
// dstDir, and absolute paths or paths containing ".." traversal are rejected.
// Only directories and regular files are supported. This makes Untar suitable
// for trusted internal layout archives and for defensive handling of archives
// produced by sibling code in this repository, but not for full-fidelity tar
// restoration where links, devices, xattrs, owners, or sparse metadata matter.
func Untar(archivePath, dstDir string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive %s: %w", archivePath, err)
	}
	defer file.Close()
	tr := tar.NewReader(file)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar %s: %w", archivePath, err)
		}
		name := filepath.Clean(header.Name)
		if name == "." || strings.HasPrefix(name, ".."+string(filepath.Separator)) || filepath.IsAbs(name) {
			return fmt.Errorf("unsafe tar path %q", header.Name)
		}
		target := filepath.Join(dstDir, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported tar entry %q type %d", header.Name, header.Typeflag)
		}
	}
}
