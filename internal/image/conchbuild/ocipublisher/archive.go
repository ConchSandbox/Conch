package ocipublisher

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// createVMSnapshotArchive packs a specific list of individual files into a flat tar.gz.
// It does not handle directories recursively and is best used for collecting
// specific log files or standalone metadata files.
func createVMSnapshotArchive(files []string) (result string, err error) {
	tempArchiveFile, err := os.CreateTemp("", "vm_snap_data_*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("failed to create temp archive file: %w", err)
	}
	result = tempArchiveFile.Name()

	defer func() {
		if err != nil {
			tempArchiveFile.Close()
			os.Remove(result)
		}
	}()

	gzw := gzip.NewWriter(tempArchiveFile)
	tw := tar.NewWriter(gzw)

	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			return "", err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return "", err
		}

		header, _ := tar.FileInfoHeader(info, filepath.Base(file))
		header.Name = filepath.Base(file)
		if err := tw.WriteHeader(header); err != nil {
			f.Close()
			return "", err
		}
		io.Copy(tw, f)
		f.Close()
	}

	tw.Close()
	gzw.Close()
	tempArchiveFile.Close()
	return result, nil
}

// createContainerLayerArchive recursively packs an entire directory tree into a tar.gz.
// It preserves Linux file modes, owners, and symbolic links, making it suitable
// for converting containerd snapshots into OCI-compliant image layers.
func createContainerLayerArchive(srcPath string) (result string, err error) {
	tempArchiveFile, err := os.CreateTemp("", "container_layer_*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("failed to create temp layer file: %w", err)
	}
	result = tempArchiveFile.Name()

	gzw := gzip.NewWriter(tempArchiveFile)
	tw := tar.NewWriter(gzw)

	// Walk through the snapshot directory recursively
	err = filepath.Walk(srcPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Generate the tar header from file system info
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}

		// IMPORTANT: OCI layers must use numeric IDs and typically belong to root
		header.Uid = 0
		header.Gid = 0
		header.Uname = "root"
		header.Gname = "root"

		// Remap the path to be relative to the snapshot root (srcPath)
		// This ensures the tar content starts from "/" instead of host absolute paths
		relPath, err := filepath.Rel(srcPath, path)
		if err != nil {
			return err
		}

		// IMPORTANT: Standardize path separators for Linux and remove "./"
		relPath = filepath.ToSlash(relPath)
		if relPath == "." {
			return nil // Skip the root directory itself to avoid "./" in tar
		}

		header.Name = relPath

		// Handle Symbolic Links (critical for shared libraries like .so files)
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header.Linkname = link
		}

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		// Only copy content for regular files (skip directories and symlinks content)
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			io.Copy(tw, f)
		}
		return nil
	})

	tw.Close()
	gzw.Close()
	tempArchiveFile.Close()

	if err != nil {
		os.Remove(result)
		return "", fmt.Errorf("failed to walk and pack directory: %w", err)
	}

	return result, nil
}
