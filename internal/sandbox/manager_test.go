package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestManagerDeleteReturnsCleanupErrors(t *testing.T) {
	wantErr := errors.New("delete cleanup marker")
	cleanup := NewCleanup()
	cleanup.Add(func(ctx context.Context) error {
		return wantErr
	})

	m := NewManager(nil, nil, 0, 0, time.Second)
	sbx := &Sandbox{
		cleanup:   cleanup,
		namespace: "default",
	}
	m.sandboxes.Store(sandboxMapKey("default", "sandbox-1"), sbx)

	err := m.Delete(SandboxDeleteRequest{SandboxId: "sandbox-1"})
	if err == nil {
		t.Fatalf("Delete() error = nil, want cleanup error")
	}
	if !strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("Delete() error = %v, want marker error", err)
	}
	if _, ok := m.sandboxes.Load(sandboxMapKey("default", "sandbox-1")); ok {
		t.Fatalf("Delete() left sandbox in manager map")
	}
}
