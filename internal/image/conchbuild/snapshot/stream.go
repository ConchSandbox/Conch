package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/opencontainers/image-spec/identity"
	"github.com/openeuler/Conch/internal/image/conchbuild/export"
	"github.com/sirupsen/logrus"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// SyncImageToContainerd exports the image from containers/storage, imports into containerd, unpacks to overlayfs.
// Returns snapshot ChainID and the imported image name (for conchd image_name-based startup).
func SyncImageToContainerd(ctx context.Context, client *containerd.Client, store storage.Store, imageID string, imageRef string, importedTag string, systemContext *types.SystemContext) (snapshotKey string, imageName string, err error) {
	tmpDir, err := os.MkdirTemp("", "buildah-containerd-*")
	if err != nil {
		return "", "", fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(tmpDir); rmErr != nil {
			logrus.Warnf("warning: failed to clean temp directory %s: %v", tmpDir, rmErr)
		}
	}()

	tmpTarPath := filepath.Join(tmpDir, "image.tar")

	exportRef := imageRef
	if exportRef == "" {
		exportRef = imageID
	}
	if importedTag == "" {
		importedTag = "buildah-oci-image:latest"
	}
	if err := export.ExportImageToTar(ctx, exportRef, tmpTarPath, "oci-archive", importedTag, systemContext, os.Stdout); err != nil {
		return "", "", fmt.Errorf("failed to export image from storage to tar: %w", err)
	}

	tarFile, err := os.Open(tmpTarPath)
	if err != nil {
		return "", "", err
	}
	defer tarFile.Close()

	importedImages, err := client.Import(ctx, tarFile)
	if err != nil {
		return "", "", fmt.Errorf("containerd import failed: %w", err)
	}

	if len(importedImages) == 0 {
		return "", "", fmt.Errorf("no images were imported")
	}

	snapshotter := client.SnapshotService("overlayfs")
	var finalSnapshotKey string
	var finalImageName string

	ordered := reorderImportedImages(importedImages, importedTag)
	for _, imgInfo := range ordered {
		img := containerd.NewImage(client, imgInfo)

		logrus.Infof("Unpacking image %s to snapshotter...", imgInfo.Name)
		err = img.Unpack(ctx, "overlayfs")
		if err != nil {
			return "", "", fmt.Errorf("failed to unpack image %s: %w", imgInfo.Name, err)
		}

		logrus.Infof("Successfully synced and unpacked. Digest: %s", imgInfo.Target.Digest)

		diffIDs, err := img.RootFS(ctx)
		if err != nil {
			return "", "", fmt.Errorf("failed to get rootfs: %w", err)
		}

		finalSnapshotKey = identity.ChainID(diffIDs).String()
		finalImageName = imgInfo.Name
		if _, err := snapshotter.Stat(ctx, finalSnapshotKey); err != nil {
			logrus.Warnf("Imported image %s unpacked but snapshot %s not found: %v", imgInfo.Name, finalSnapshotKey, err)
			continue
		}

		logrus.Infof("Unpack successful. Snapshot ChainID: %s", finalSnapshotKey)
		break
	}

	if finalSnapshotKey == "" {
		return "", "", fmt.Errorf("no snapshot key generated")
	}

	logrus.Info("Image sync and snapshot creation completed successfully")
	return finalSnapshotKey, finalImageName, nil
}

func reorderImportedImages(imported []images.Image, preferredName string) []images.Image {
	if preferredName == "" || len(imported) <= 1 {
		return imported
	}
	out := make([]images.Image, 0, len(imported))
	for _, img := range imported {
		if img.Name == preferredName {
			out = append(out, img)
		}
	}
	for _, img := range imported {
		if img.Name != preferredName {
			out = append(out, img)
		}
	}
	return out
}
