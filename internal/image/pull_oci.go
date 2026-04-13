package image

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/images"
	"github.com/openeuler/Conch/internal/image/conchbuild/buildahcli"
	"github.com/openeuler/Conch/internal/image/conchbuild/erofs"
	"github.com/openeuler/Conch/internal/image/conchbuild/export"
	"github.com/openeuler/Conch/internal/image/conchbuild/ocipublisher"
	"github.com/openeuler/Conch/internal/image/conchbuild/rootfs"
	"github.com/sirupsen/logrus"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

type PullOCIImageOptions struct {
	SourceImage            string
	DefaultKernelImage     string
	SourcePlainHTTP        bool
	SourceRegistryUsername string
	SourceRegistryPassword string
	KernelPlainHTTP        bool
	KernelRegistryUsername string
	KernelRegistryPassword string
	Platform               string
}

// PullAndUnpackOCIImage converts a standard OCI image into a local Conch sandbox-image,
// imports it into containerd, and unpacks the child images.
func PullAndUnpackOCIImage(ctx context.Context, ctrdClient *containerd.Client, opts PullOCIImageOptions) (map[string]string, error) {
	if opts.SourceImage == "" {
		return nil, fmt.Errorf("source image is required")
	}
	if opts.DefaultKernelImage == "" {
		return nil, fmt.Errorf("default kernel image is required for OCI conversion")
	}

	storeOpts, err := storage.DefaultStoreOptions()
	if err != nil {
		return nil, fmt.Errorf("default store options: %w", err)
	}
	store, err := storage.GetStore(storeOpts)
	if err != nil {
		return nil, fmt.Errorf("get store: %w", err)
	}
	defer func() { _, _ = store.Shutdown(false) }()

	platform := opts.Platform
	if platform == "" {
		platform = defaultPullPlatform()
	}

	if err := ensureLocalImage(ctx, localImagePullOptions{
		ImageRef:  opts.SourceImage,
		PlainHTTP: opts.SourcePlainHTTP,
		Username:  opts.SourceRegistryUsername,
		Password:  opts.SourceRegistryPassword,
		Platform:  platform,
	}); err != nil {
		return nil, fmt.Errorf("pull source image into local storage: %w", err)
	}
	if err := ensureLocalImage(ctx, localImagePullOptions{
		ImageRef:  opts.DefaultKernelImage,
		PlainHTTP: opts.KernelPlainHTTP,
		Username:  opts.KernelRegistryUsername,
		Password:  opts.KernelRegistryPassword,
		Platform:  platform,
	}); err != nil {
		return nil, fmt.Errorf("pull kernel image into local storage: %w", err)
	}

	srcImg, err := store.Image(opts.SourceImage)
	if err != nil {
		return nil, fmt.Errorf("lookup source image %s: %w", opts.SourceImage, err)
	}

	tmpDir, err := os.MkdirTemp("", "conch-pull-oci-*")
	if err != nil {
		return nil, fmt.Errorf("create temp workspace: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	erofsOut := filepath.Join(tmpDir, "erofs")
	rootfsLayer, err := erofs.ConvertImageToEROFSDirect(ctx, store, srcImg.ID, opts.SourceImage, &types.SystemContext{}, erofsOut)
	if err != nil {
		return nil, fmt.Errorf("convert OCI image to EROFS: %w", err)
	}

	suffix := fmt.Sprintf("pull-%d", time.Now().UnixNano())
	rootfsTag := "localhost/conch/pmem-rootfs:" + suffix
	if _, err := rootfs.BuildRootfsImage(ctx, store, []string{rootfsLayer}, rootfsTag); err != nil {
		return nil, fmt.Errorf("build rootfs image: %w", err)
	}
	kernelTag := "localhost/conch/kernel:" + suffix
	if err := tagLocalImage(ctx, opts.DefaultKernelImage, kernelTag); err != nil {
		return nil, fmt.Errorf("retag kernel image: %w", err)
	}

	sandboxTag := deriveSandboxImageRef(opts.SourceImage)
	if _, err := ocipublisher.PublishSandboxImage(ctx, store, rootfsTag, kernelTag, sandboxTag); err != nil {
		return nil, fmt.Errorf("publish sandbox image: %w", err)
	}
	fmt.Printf("Generated sandbox image: %s\n", sandboxTag)

	importedName, err := importImageFromStorage(ctx, ctrdClient, sandboxTag)
	if err != nil {
		return nil, fmt.Errorf("import sandbox image to containerd: %w", err)
	}

	if err := ValidateConchImageIndex(ctx, ctrdClient, importedName); err != nil {
		return nil, fmt.Errorf("validate imported sandbox image: %w", err)
	}
	return UnpackAllSubImages(ctx, ctrdClient, importedName)
}

type localImagePullOptions struct {
	ImageRef  string
	PlainHTTP bool
	Username  string
	Password  string
	Platform  string
}

func defaultPullPlatform() string {
	return "linux/" + runtime.GOARCH
}

func ensureLocalImage(ctx context.Context, opts localImagePullOptions) error {
	if opts.ImageRef == "" {
		return fmt.Errorf("image reference is required")
	}

	args := []string{"pull"}
	if opts.PlainHTTP {
		args = append(args, "--tls-verify=false")
	}
	if opts.Username != "" || opts.Password != "" {
		args = append(args, "--creds", opts.Username+":"+opts.Password)
	}
	if opts.Platform != "" {
		args = append(args, "--platform", opts.Platform)
	}
	args = append(args, opts.ImageRef)

	cmd := exec.CommandContext(ctx, buildahcli.Bin(), args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("buildah pull %s: %w: %s", opts.ImageRef, err, strings.TrimSpace(string(out)))
	}
	logrus.Infof("buildah pull completed for %s platform=%s", opts.ImageRef, opts.Platform)
	return nil
}

func tagLocalImage(ctx context.Context, srcRef, destRef string) error {
	if srcRef == "" || destRef == "" {
		return fmt.Errorf("both source and destination image references are required")
	}

	cmd := exec.CommandContext(ctx, buildahcli.Bin(), "tag", srcRef, destRef)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("buildah tag %s -> %s: %w: %s", srcRef, destRef, err, strings.TrimSpace(string(out)))
	}
	logrus.Infof("buildah tag completed for %s -> %s", srcRef, destRef)
	return nil
}

func importImageFromStorage(ctx context.Context, ctrdClient *containerd.Client, imageRef string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "conch-import-*")
	if err != nil {
		return "", fmt.Errorf("create temp import dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpTarPath := filepath.Join(tmpDir, "image.tar")
	if err := exportManifestToTar(ctx, imageRef, tmpTarPath); err != nil {
		return "", fmt.Errorf("export image %s: %w", imageRef, err)
	}

	tarFile, err := os.Open(tmpTarPath)
	if err != nil {
		return "", err
	}
	defer tarFile.Close()

	importedImages, err := ctrdClient.Import(ctx, tarFile)
	if err != nil {
		return "", fmt.Errorf("containerd import failed: %w", err)
	}
	if len(importedImages) == 0 {
		return "", fmt.Errorf("no images were imported")
	}

	for _, img := range importedImages {
		if img.Name == imageRef {
			return imageRef, nil
		}
	}
	for _, img := range importedImages {
		if img.Target.MediaType == images.MediaTypeDockerSchema2ManifestList || img.Target.MediaType == "application/vnd.oci.image.index.v1+json" {
			return img.Name, nil
		}
	}
	return importedImages[0].Name, nil
}

func exportManifestToTar(ctx context.Context, imageRef, destPath string) error {
	args := []string{
		"manifest", "push",
		"--all",
		imageRef,
		"oci-archive:" + destPath + ":" + imageRef,
	}

	cmd := exec.CommandContext(ctx, buildahcli.Bin(), args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		logrus.Infof("buildah manifest push completed for %s", imageRef)
		return nil
	}

	logrus.Warnf("buildah manifest push failed for %s, falling back to containers-storage export: %v %s", imageRef, err, strings.TrimSpace(string(out)))
	if fallbackErr := export.ExportImageToTar(ctx, imageRef, destPath, "oci-archive", imageRef, &types.SystemContext{}, nil); fallbackErr != nil {
		return fmt.Errorf("buildah manifest push failed: %w; fallback export failed: %v", err, fallbackErr)
	}
	return nil
}

var invalidImageRefChar = regexp.MustCompile(`[^a-z0-9._-]+`)

func deriveSandboxImageRef(sourceImage string) string {
	ref := sourceImage
	tag := "latest"

	if at := strings.IndexByte(ref, '@'); at >= 0 {
		ref = ref[:at]
	}

	lastSlash := strings.LastIndexByte(ref, '/')
	lastColon := strings.LastIndexByte(ref, ':')
	if lastColon > lastSlash {
		tag = ref[lastColon+1:]
		ref = ref[:lastColon]
	}

	name := ref
	if lastSlash >= 0 {
		name = ref[lastSlash+1:]
	}
	name = sanitizeRefComponent(name, "image")
	tag = sanitizeRefComponent(tag, "latest")

	return fmt.Sprintf("localhost/conch/sandbox-image-%s:%s", name, tag)
}

func sanitizeRefComponent(input, fallback string) string {
	value := strings.ToLower(strings.TrimSpace(input))
	value = invalidImageRefChar.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-.")
	if value == "" {
		return fallback
	}
	return value
}
