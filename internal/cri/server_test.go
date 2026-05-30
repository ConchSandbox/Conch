package cri

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestRuntimeBasics(t *testing.T) {
	svc := &service{}

	version, err := svc.Version(context.Background(), &runtimev1.VersionRequest{Version: "v1"})
	if err != nil {
		t.Fatalf("Version() error = %v", err)
	}
	if version.GetRuntimeName() != runtimeName || version.GetRuntimeApiVersion() != runtimeAPIVersion {
		t.Fatalf("Version() = %#v", version)
	}

	status, err := svc.Status(context.Background(), &runtimev1.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if len(status.GetStatus().GetConditions()) != 2 {
		t.Fatalf("Status() conditions = %#v", status.GetStatus().GetConditions())
	}

	cfg, err := svc.RuntimeConfig(context.Background(), &runtimev1.RuntimeConfigRequest{})
	if err != nil {
		t.Fatalf("RuntimeConfig() error = %v", err)
	}
	if cfg.GetLinux().GetCgroupDriver() != runtimev1.CgroupDriver_SYSTEMD {
		t.Fatalf("RuntimeConfig() = %#v", cfg)
	}
}

func TestRemoveStaleUnixSocketRejectsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conch-cri.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	err := removeStaleUnixSocket(path)
	if err == nil {
		t.Fatalf("removeStaleUnixSocket() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "not a unix socket") {
		t.Fatalf("error = %q, want non-socket error", err.Error())
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("regular file was removed: %v", statErr)
	}
}
