package erofs

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	// AlignBytes is the alignment for EROFS output (2MB)
	AlignBytes = 2 * 1024 * 1024
)

// MkfsErofsPath is the path to mkfs.erofs. Override via MKFS_EROFS env.
var MkfsErofsPath = "mkfs.erofs"

func init() {
	if p := os.Getenv("MKFS_EROFS"); p != "" {
		MkfsErofsPath = p
	}
}

func checkMkfsErofs() error {
	if _, err := exec.LookPath(MkfsErofsPath); err != nil {
		return fmt.Errorf("mkfs.erofs not found (install erofs-utils: apt install erofs-utils or dnf install erofs-utils): %w", err)
	}
	return nil
}

// DirToEROFS converts a directory directly to EROFS format with 2MB alignment.
// Uses: mkfs.erofs -Enoinline_data dest.erofs src_dir/
// No tar intermediary - direct directory input.
func DirToEROFS(srcDir, destErofs string) error {
	if err := checkMkfsErofs(); err != nil {
		return err
	}
	cmd := exec.Command(MkfsErofsPath, "-Enoinline_data", destErofs, srcDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkfs.erofs failed: %w", err)
	}
	return alignTo2MB(destErofs)
}

// TarToEROFS converts a tar file to EROFS format with 2MB alignment.
// Uses: mkfs.erofs --tar=f --aufs -Enoinline_data dest.erofs src.tar
// Then truncates dest to next 2MB boundary.
func TarToEROFS(srcTar, destErofs string) error {
	if err := checkMkfsErofs(); err != nil {
		return err
	}
	cmd := exec.Command(MkfsErofsPath, "--tar=f", "--aufs", "-Enoinline_data", destErofs, srcTar)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mkfs.erofs failed: %w", err)
	}
	return alignTo2MB(destErofs)
}

// alignTo2MB truncates the file to the next 2MB boundary (round up).
func alignTo2MB(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	size := info.Size()
	if size == 0 {
		size = AlignBytes
	}
	aligned := ((size + AlignBytes - 1) / AlignBytes) * AlignBytes
	return os.Truncate(path, aligned)
}

// ConvertTarToEROFS converts a single tar (or tar.gz/tar.xz) to EROFS.
// If src is compressed, decompresses to temp tar first.
func ConvertTarToEROFS(srcPath, destDir, baseName string) (string, error) {
	workDir, err := os.MkdirTemp("", "buildah-erofs-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(workDir)

	srcTar := srcPath
	ext := filepath.Ext(srcPath)
	if ext == ".xz" || ext == ".gz" {
		srcTar = filepath.Join(workDir, "layer.tar")
		var cmd *exec.Cmd
		if ext == ".xz" {
			cmd = exec.Command("xz", "-dc", srcPath)
		} else {
			cmd = exec.Command("gzip", "-dc", srcPath)
		}
		out, err := os.Create(srcTar)
		if err != nil {
			return "", err
		}
		defer out.Close()
		cmd.Stdout = out
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("decompress failed: %w", err)
		}
	}

	destErofs := filepath.Join(destDir, baseName+".erofs")
	if err := TarToEROFS(srcTar, destErofs); err != nil {
		return "", err
	}
	return destErofs, nil
}
