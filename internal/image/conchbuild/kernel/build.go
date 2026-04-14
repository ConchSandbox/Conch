package kernel

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/openeuler/Conch/internal/image/conchbuild/buildahcli"
	"github.com/openeuler/Conch/internal/image/conchbuild/client"
	"github.com/sirupsen/logrus"
	"go.podman.io/storage"
)

// Kernel image labels.
const (
	LabelType   = "io.conch.type=combined"
	LabelKernel = "io.conch.kernel=bzImage"
	LabelInitrd = "io.conch.initrd=present"
)

// BuildKernelImage builds the kernel image from scratch by copying
// the kernel and initrd into a scratch image, setting the required labels, and
// committing the result.
func BuildKernelImage(ctx context.Context, store storage.Store, contextDir, kernelFile, initrdFile, tag string) (string, error) {
	_ = store

	kernelPath, initrdPath, err := client.ResolveKernelPaths(contextDir, kernelFile, initrdFile)
	if err != nil {
		return "", fmt.Errorf("resolving kernel paths: %w", err)
	}

	commitRef := tag
	if commitRef == "" {
		commitRef = "localhost/conch/kernel:latest"
	}

	bin := buildahcli.Bin()

	fromCmd := exec.CommandContext(ctx, bin, "from", "scratch")
	fromCmd.Stderr = os.Stderr
	fromOut, err := fromCmd.Output()
	if err != nil {
		return "", fmt.Errorf("buildah from scratch: %w", err)
	}
	cid := strings.TrimSpace(string(fromOut))
	if cid == "" {
		return "", fmt.Errorf("buildah from scratch returned empty container id")
	}
	defer func() {
		rm := exec.CommandContext(ctx, bin, "rm", cid)
		rm.Stderr = os.Stderr
		if out, err := rm.CombinedOutput(); err != nil {
			logrus.Warnf("buildah rm %s: %v %s", cid, err, strings.TrimSpace(string(out)))
		}
	}()

	for _, item := range []struct {
		src string
		dst string
	}{
		{kernelPath, "/boot/vmlinuz"},
		{initrdPath, "/data/conch.initrd"},
	} {
		copyCmd := exec.CommandContext(ctx, bin, "copy", cid, item.src, item.dst)
		copyCmd.Stdout = os.Stdout
		copyCmd.Stderr = os.Stderr
		if err := copyCmd.Run(); err != nil {
			return "", fmt.Errorf("buildah copy %s -> %s: %w", item.src, item.dst, err)
		}
	}

	configCmd := exec.CommandContext(ctx, bin,
		"config",
		"--label", LabelType,
		"--label", LabelKernel,
		"--label", LabelInitrd,
		cid,
	)
	configCmd.Stderr = os.Stderr
	if err := configCmd.Run(); err != nil {
		return "", fmt.Errorf("buildah config kernel image: %w", err)
	}

	commitCmd := exec.CommandContext(ctx, bin, "commit", cid, commitRef)
	commitCmd.Stderr = os.Stderr
	if out, err := commitCmd.Output(); err != nil {
		return "", fmt.Errorf("buildah commit kernel image: %w", err)
	} else if strings.TrimSpace(string(out)) != "" {
		logrus.Debugf("buildah commit kernel image output: %s", strings.TrimSpace(string(out)))
	}

	img, err := store.Image(commitRef)
	if err != nil {
		return "", fmt.Errorf("lookup committed kernel image %s: %w", commitRef, err)
	}
	imageID := strings.TrimSpace(img.ID)
	if imageID == "" {
		return "", fmt.Errorf("empty image id for committed kernel image %s", commitRef)
	}
	logrus.Infof("Built kernel image: %s (ID: %s)", commitRef, imageID)
	return imageID, nil
}
