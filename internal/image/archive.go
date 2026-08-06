package image

import (
	"context"
	"fmt"
	"io"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

func ImportArchive(ctx context.Context, client *containerdclient.Client, reader io.Reader, req runtimeapi.ImportImageArchiveOptions) (runtimeapi.ImportImageArchiveResult, error) {
	if client == nil || client.Client == nil {
		return runtimeapi.ImportImageArchiveResult{}, fmt.Errorf("containerd client is required")
	}
	if reader == nil {
		return runtimeapi.ImportImageArchiveResult{}, fmt.Errorf("%w: archive is required", ErrInvalidRequest)
	}

	importCtx := containerdclient.NewNamespaceContext(ctx)
	importOpts := []containerd.ImportOpt{}
	if req.ImportedTag != "" {
		importOpts = append(importOpts, containerd.WithIndexName(req.ImportedTag))
	}
	importedImages, err := client.Import(importCtx, reader, importOpts...)
	if err != nil {
		return runtimeapi.ImportImageArchiveResult{}, fmt.Errorf("containerd import failed: %w", err)
	}
	if len(importedImages) == 0 {
		return runtimeapi.ImportImageArchiveResult{}, fmt.Errorf("no images were imported")
	}
	for _, imported := range importedImages {
		kind := DetectImageKind(importCtx, client.ContentStore(), imported.Target)
		if err := SetImageKindLabel(importCtx, client.ImageService(), imported.Name, kind); err != nil {
			return runtimeapi.ImportImageArchiveResult{}, err
		}
	}

	snapshotter := client.SnapshotService("erofs")
	finalSnapshotKey, finalImageName, err := selectImportedSnapshot(
		reorderImportedImages(importedImages, req.ImportedTag),
		func(imgInfo images.Image) (map[string]string, bool, error) {
			if err := ValidateBootIndexContent(importCtx, client.Client, imgInfo.Name); err != nil {
				return nil, false, nil
			}
			snapshotMap, err := UnpackAllSubImages(importCtx, client.Client, imgInfo.Name)
			if err != nil {
				return nil, true, fmt.Errorf("failed to unpack Conch image index %s: %w", imgInfo.Name, err)
			}
			return snapshotMap, true, nil
		},
		func(imgInfo images.Image) (string, error) {
			img := containerd.NewImage(client.Client, imgInfo)
			if err := img.Unpack(importCtx, "erofs"); err != nil {
				return "", fmt.Errorf("failed to unpack image %s: %w", imgInfo.Name, err)
			}

			diffIDs, err := img.RootFS(importCtx)
			if err != nil {
				return "", fmt.Errorf("failed to get rootfs: %w", err)
			}
			snapshotKey := identity.ChainID(diffIDs).String()
			if _, err := snapshotter.Stat(importCtx, snapshotKey); err != nil {
				return "", nil
			}
			return snapshotKey, nil
		},
	)
	if err != nil {
		return runtimeapi.ImportImageArchiveResult{}, err
	}
	if finalSnapshotKey == "" {
		return runtimeapi.ImportImageArchiveResult{}, fmt.Errorf("no snapshot key generated")
	}
	return runtimeapi.ImportImageArchiveResult{
		SnapshotKey: finalSnapshotKey,
		ImageName:   finalImageName,
	}, nil
}

func ExportArchive(ctx context.Context, client *containerdclient.Client, writer io.Writer, req runtimeapi.ExportImageArchiveOptions) error {
	if client == nil || client.Client == nil {
		return fmt.Errorf("containerd client is required")
	}
	if writer == nil {
		return fmt.Errorf("%w: archive writer is required", ErrInvalidRequest)
	}
	if req.ImageName == "" {
		return fmt.Errorf("%w: image_name is required", ErrInvalidRequest)
	}

	exportCtx := containerdclient.NewNamespaceContext(ctx)
	if _, err := client.ImageService().Get(exportCtx, req.ImageName); err != nil {
		return fmt.Errorf("lookup image %s: %w", req.ImageName, err)
	}
	if err := client.Export(exportCtx, writer, archive.WithImage(client.ImageService(), req.ImageName)); err != nil {
		return fmt.Errorf("export image %s: %w", req.ImageName, err)
	}
	return nil
}

func selectImportedSnapshot(importedImages []images.Image, unpackConchIndex func(images.Image) (map[string]string, bool, error), unpackRegularImage func(images.Image) (string, error)) (string, string, error) {
	for _, imgInfo := range importedImages {
		if imgInfo.Target.MediaType == ocispec.MediaTypeImageIndex {
			snapshotMap, isConchIndex, err := unpackConchIndex(imgInfo)
			if err != nil {
				return "", "", err
			}
			if isConchIndex {
				return snapshotMap[KindRootfs], imgInfo.Name, nil
			}
		}
		snapshotKey, err := unpackRegularImage(imgInfo)
		if err != nil {
			return "", "", err
		}
		if snapshotKey == "" {
			continue
		}
		return snapshotKey, imgInfo.Name, nil
	}
	return "", "", nil
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
