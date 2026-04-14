package erofs

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/openeuler/Conch/internal/image/conchbuild/export"
	"github.com/sirupsen/logrus"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// dockerManifestEntry is one entry in docker save manifest.json
type dockerManifestEntry struct {
	Config   string   `json:"Config"`
	Layers   []string `json:"Layers"`
	RepoTags []string `json:"RepoTags"`
}

// ConvertImageToEROFS exports the OCI image from storage to a temp archive,
// then converts each layer tar to EROFS. Returns paths to layer0.erofs, layer1.erofs, etc.
func ConvertImageToEROFS(store storage.Store, imageID string, systemContext *types.SystemContext, outDir string) ([]string, error) {
	_ = store // reserved for future custom store support; export uses process default
	tmpDir, err := os.MkdirTemp("", "buildah-oci-erofs-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	tmpTar := filepath.Join(tmpDir, "image.tar")
	if err := export.ExportImageToTar(context.Background(), imageID, tmpTar, "docker-archive", "conch", systemContext, nil); err != nil {
		return nil, err
	}

	return convertDockerArchiveToEROFS(tmpTar, outDir)
}

// convertDockerArchiveToEROFS reads a docker-archive tar, extracts to temp, then
// converts each layer tar to EROFS. Returns paths to layer0.erofs, layer1.erofs, etc.
func convertDockerArchiveToEROFS(archivePath, outDir string) ([]string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	extractDir, err := os.MkdirTemp("", "buildah-docker-archive-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(extractDir)

	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	var manifestData []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(extractDir, hdr.Name)
		// Prevent path traversal: ensure dst stays within extractDir
		absExtract, _ := filepath.Abs(extractDir)
		absDst, err := filepath.Abs(dst)
		if err != nil {
			return nil, fmt.Errorf("invalid path %s: %w", hdr.Name, err)
		}
		rel, err := filepath.Rel(absExtract, absDst)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("tar path escapes extract dir: %s", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeDir {
			os.MkdirAll(dst, 0o755)
			continue
		}
		if base := filepath.Base(hdr.Name); base == "manifest.json" {
			manifestData, err = io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			continue
		}
		os.MkdirAll(filepath.Dir(dst), 0o755)
		outFile, err := os.Create(dst)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(outFile, tr); err != nil {
			outFile.Close()
			return nil, err
		}
		outFile.Close()
	}

	if len(manifestData) == 0 {
		return nil, fmt.Errorf("no manifest.json in archive")
	}

	var manifests []dockerManifestEntry
	if err := json.Unmarshal(manifestData, &manifests); err != nil {
		return nil, fmt.Errorf("parse manifest.json: %w", err)
	}
	if len(manifests) == 0 {
		return nil, fmt.Errorf("empty manifest")
	}

	layers := manifests[0].Layers
	var result []string
	for i, layerPath := range layers {
		srcPath := filepath.Join(extractDir, filepath.FromSlash(layerPath))
		if _, err := os.Stat(srcPath); err != nil {
			return nil, fmt.Errorf("layer not found %s: %w", layerPath, err)
		}
		baseName := fmt.Sprintf("layer%d", i)
		if i == 0 && len(layers) == 1 {
			baseName = "rootfs"
		}
		destPath, err := ConvertTarToEROFS(srcPath, outDir, baseName)
		if err != nil {
			return nil, fmt.Errorf("convert layer %d: %w", i, err)
		}
		result = append(result, destPath)
		logrus.Infof("Converted layer %d to %s", i, destPath)
	}
	return result, nil
}

