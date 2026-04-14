package rootfs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/openeuler/Conch/internal/image/conchbuild/buildahcli"
	"github.com/sirupsen/logrus"
	"go.podman.io/storage"
)

const (
	LabelType   = "io.conch.type=pmem-rootfs"
	LabelFormat = "io.conch.format=erofs"
)

// BuildRootfsImage builds a pmem-rootfs image from one or more local EROFS layer files.
// Each input file is copied into the image root as /<basename>.
func BuildRootfsImage(ctx context.Context, store storage.Store, erofsPaths []string, tag string) (string, error) {
	_ = store

	if len(erofsPaths) == 0 {
		return "", fmt.Errorf("no EROFS layers provided")
	}

	commitRef := tag
	if commitRef == "" {
		commitRef = "localhost/conch/pmem-rootfs:latest"
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

	cleanup := func() {
		rm := exec.CommandContext(ctx, bin, "rm", cid)
		rm.Stderr = os.Stderr
		if out, err := rm.CombinedOutput(); err != nil {
			logrus.Warnf("buildah rm %s: %v %s", cid, err, strings.TrimSpace(string(out)))
		}
	}
	defer cleanup()

	layerNames := make([]string, 0, len(erofsPaths))
	for _, layerPath := range erofsPaths {
		if _, err := os.Stat(layerPath); err != nil {
			return "", fmt.Errorf("stat EROFS layer %s: %w", layerPath, err)
		}
		layerName := filepath.Base(layerPath)
		layerNames = append(layerNames, layerName)

		copyCmd := exec.CommandContext(ctx, bin, "copy", cid, layerPath, "/"+layerName)
		copyCmd.Stdout = os.Stdout
		copyCmd.Stderr = os.Stderr
		if err := copyCmd.Run(); err != nil {
			return "", fmt.Errorf("buildah copy %s: %w", layerPath, err)
		}
	}

	labelArgs := []string{
		"config",
		"--label", LabelType,
		"--label", LabelFormat,
		"--label", "io.conch.layers=" + strings.Join(layerNames, ","),
		"--annotation", "description=PMEM rootfs image containing " + strings.Join(layerNames, ","),
		cid,
	}
	configCmd := exec.CommandContext(ctx, bin, labelArgs...)
	configCmd.Stderr = os.Stderr
	if err := configCmd.Run(); err != nil {
		return "", fmt.Errorf("buildah config rootfs image: %w", err)
	}

	commitCmd := exec.CommandContext(ctx, bin, "commit", cid, commitRef)
	commitCmd.Stderr = os.Stderr
	if out, err := commitCmd.Output(); err != nil {
		return "", fmt.Errorf("buildah commit rootfs image: %w", err)
	} else if strings.TrimSpace(string(out)) != "" {
		logrus.Debugf("buildah commit rootfs image output: %s", strings.TrimSpace(string(out)))
	}

	logrus.Infof("Built pmem-rootfs image: %s", commitRef)
	return commitRef, nil
}
