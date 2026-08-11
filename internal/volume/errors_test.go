package volume

import (
	"errors"
	"testing"

	"github.com/openeuler/Conch/internal/apperror"
)

func TestPrepareSandboxClassifiesInvalidMount(t *testing.T) {
	manager := &Manager{maxMounts: 1}
	_, err := manager.PrepareSandbox("sandbox-a", []Mount{{Source: "relative", Path: "/data"}})
	if !errors.Is(err, ErrInvalidMount) {
		t.Fatalf("error = %v, want ErrInvalidMount", err)
	}
	var appErr *apperror.Error
	if !errors.As(err, &appErr) || appErr.Code() != "volume.invalid_mount" {
		t.Fatalf("application error = %#v", appErr)
	}
}
