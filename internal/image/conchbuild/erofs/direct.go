package erofs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/openeuler/Conch/internal/image/conchbuild/buildahcli"
	"github.com/sirupsen/logrus"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// ConvertImageToEROFSDirect mounts the image rootfs via `buildah` CLI and runs mkfs.erofs
// on the merged directory. No tar export or per-layer conversion.
// The image must already exist in the same containers/storage graphroot that the buildah binary uses.
func ConvertImageToEROFSDirect(ctx context.Context, store storage.Store, imageID string, imageRef string, systemContext *types.SystemContext, outDir string) (string, error) {
	_ = store
	_ = systemContext

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}

	cname := fmt.Sprintf("erofs-tmp-%d", time.Now().UnixNano())
	sourceRef := imageRef
	if sourceRef == "" {
		sourceRef = imageID
	}
	sourceRef = "containers-storage:" + sourceRef
	bin := buildahcli.Bin()

	fromCmd := exec.CommandContext(ctx, bin, "from", "--pull-never", "--name", cname, sourceRef)
	fromOut, fromErr := fromCmd.CombinedOutput()
	if fromErr != nil {
		return "", fmt.Errorf("buildah from: %w: %s", fromErr, strings.TrimSpace(string(fromOut)))
	}

	defer func() {
		un := exec.CommandContext(ctx, bin, "unmount", cname)
		un.Stderr = os.Stderr
		if err := un.Run(); err != nil {
			logrus.Warnf("buildah unmount %s: %v", cname, err)
		}
		rm := exec.CommandContext(ctx, bin, "rm", cname)
		rm.Stderr = os.Stderr
		if out, err := rm.CombinedOutput(); err != nil {
			logrus.Warnf("buildah rm %s: %v %s", cname, err, strings.TrimSpace(string(out)))
		}
	}()

	mountCmd := exec.CommandContext(ctx, bin, "mount", cname)
	mountOut, err := mountCmd.Output()
	if err != nil {
		return "", fmt.Errorf("buildah mount: %w", err)
	}
	mountPoint := strings.TrimSpace(string(mountOut))
	if mountPoint == "" {
		return "", fmt.Errorf("buildah mount: empty mount path")
	}

	destErofs := filepath.Join(outDir, "layer0.erofs")
	if err := DirToEROFS(mountPoint, destErofs); err != nil {
		return "", err
	}
	logrus.Infof("Direct OCI→EROFS conversion complete: %s (from mounted rootfs)", destErofs)
	return destErofs, nil
}
