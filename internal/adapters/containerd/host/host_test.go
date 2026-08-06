package containerdhost

import (
	"context"
	"strings"
	"testing"

	"github.com/containerd/plugin/registry"
)

func TestStartAndClose(t *testing.T) {
	rootDir := t.TempDir()
	stateDir := t.TempDir()

	host, err := Start(context.Background(), Config{
		RootDir:  rootDir,
		StateDir: stateDir,
		Snapshot: SnapshotConfig{
			WorkDir: t.TempDir(),
		},
	})
	if err != nil {
		if embeddedEROFSUnavailable(err) {
			t.Skipf("erofs snapshotter unavailable in test environment: %v", err)
		}
		t.Fatalf("start host: %v", err)
	}
	if host.Client() == nil {
		t.Fatal("client is nil")
	}
	if host.SnapshotServer() == nil {
		t.Fatal("snapshot server is nil")
	}
	if _, err := host.Client().NamespaceService().List(context.Background()); err != nil {
		t.Fatalf("list namespaces: %v", err)
	}
	if err := host.Close(); err != nil {
		t.Fatalf("close host: %v", err)
	}

	host, err = Start(context.Background(), Config{
		RootDir:  t.TempDir(),
		StateDir: t.TempDir(),
		Snapshot: SnapshotConfig{
			WorkDir: t.TempDir(),
		},
	})
	if err != nil {
		if embeddedEROFSUnavailable(err) {
			t.Skipf("erofs snapshotter unavailable in test environment: %v", err)
		}
		t.Fatalf("restart host: %v", err)
	}
	if err := host.Close(); err != nil {
		t.Fatalf("close restarted host: %v", err)
	}
}

func embeddedEROFSUnavailable(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "containerd snapshotter is nil") ||
		strings.Contains(message, "EROFS unsupported") ||
		strings.Contains(message, "no plugins registered for io.containerd.snapshotter.v1")
}

func TestOnlyHostConchPluginRegistered(t *testing.T) {
	var got []string
	for _, registration := range registry.Graph(nil) {
		if strings.HasPrefix(registration.Type.String(), "io.conch.") {
			got = append(got, registration.URI())
		}
	}
	if len(got) != 1 || got[0] != pluginURI {
		t.Fatalf("registered Conch plugins = %v, want [%s]", got, pluginURI)
	}
}
