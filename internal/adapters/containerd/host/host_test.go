package containerdhost

import (
	"context"
	"strings"
	"testing"
)

func TestStartAndClose(t *testing.T) {
	rootDir := t.TempDir()
	stateDir := t.TempDir()

	host, err := Start(context.Background(), Config{
		RootDir:          rootDir,
		StateDir:         stateDir,
		DefaultNamespace: "test",
		Snapshot: SnapshotConfig{
			Enabled: true,
			WorkDir: t.TempDir(),
		},
	})
	if err != nil {
		if strings.Contains(err.Error(), "containerd snapshotter is nil") ||
			strings.Contains(err.Error(), "EROFS unsupported") {
			t.Skipf("erofs snapshotter unavailable in test environment: %v", err)
		}
		t.Fatalf("start host: %v", err)
	}
	if host.Client() == nil {
		t.Fatal("client is nil")
	}
	if host.ImageService() == nil {
		t.Fatal("image service is nil")
	}
	if host.SnapshotService() == nil {
		t.Fatal("snapshot service is nil")
	}
	if got := host.Client().DefaultNamespace(); got != "test" {
		t.Fatalf("default namespace = %q, want %q", got, "test")
	}
	if _, err := host.Client().NamespaceService().List(context.Background()); err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if err := host.Close(); err != nil {
		t.Fatalf("close host: %v", err)
	}

	host, err = Start(context.Background(), Config{
		RootDir:          t.TempDir(),
		StateDir:         t.TempDir(),
		DefaultNamespace: "test",
		Snapshot: SnapshotConfig{
			Enabled: true,
			WorkDir: t.TempDir(),
		},
	})
	if err != nil {
		if strings.Contains(err.Error(), "containerd snapshotter is nil") ||
			strings.Contains(err.Error(), "EROFS unsupported") {
			t.Skipf("erofs snapshotter unavailable in test environment: %v", err)
		}
		t.Fatalf("restart host: %v", err)
	}
	if err := host.Close(); err != nil {
		t.Fatalf("close restarted host: %v", err)
	}
}

func TestImagePluginConfig(t *testing.T) {
	got := imagePluginConfig(ImageConfig{
		DefaultKernelImage:            "registry.example.invalid/conch/kernel:6.6.0",
		DefaultKernelPlainHTTP:        true,
		DefaultKernelRegistryUsername: "kernel-user",
		DefaultKernelRegistryPassword: "kernel-pass",
	})

	if got["default_kernel_image"] != "registry.example.invalid/conch/kernel:6.6.0" {
		t.Fatalf("default_kernel_image = %#v", got["default_kernel_image"])
	}
	if got["default_kernel_plain_http"] != true {
		t.Fatalf("default_kernel_plain_http = %#v", got["default_kernel_plain_http"])
	}
	if got["default_kernel_registry_username"] != "kernel-user" {
		t.Fatalf("default_kernel_registry_username = %#v", got["default_kernel_registry_username"])
	}
	if got["default_kernel_registry_password"] != "kernel-pass" {
		t.Fatalf("default_kernel_registry_password = %#v", got["default_kernel_registry_password"])
	}
}
