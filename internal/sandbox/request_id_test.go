package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/openeuler/Conch/pkg/requestid"
)

type correlatedBootPreparer struct {
	BootPreparer
	prepareID  string
	releaseID  string
	releaseErr error
}

func (b *correlatedBootPreparer) Prepare(ctx context.Context, req PrepareBootRequest) (PreparedBoot, error) {
	b.prepareID = requestid.FromContext(ctx)
	return b.BootPreparer.Prepare(ctx, req)
}

func (b *correlatedBootPreparer) Release(ctx context.Context, req ReleaseBootRequest) error {
	b.releaseID = requestid.FromContext(ctx)
	b.releaseErr = ctx.Err()
	return b.BootPreparer.Release(ctx, req)
}

func TestSandboxCreateAndRollbackPropagateRequestID(t *testing.T) {
	for _, fail := range []bool{false, true} {
		b := &correlatedBootPreparer{BootPreparer: readyBoot()}
		m, _, _ := newLifecycleTestManager(t, b)
		ctx, cancel := context.WithCancel(requestid.With(context.Background(), "create-request-206"))
		defer cancel()
		launch := m.launch
		m.launch = func(ctx context.Context, req CreateRequest, spec VMStartSpec, ids createRuntimeIDs, restore bool) (*Sandbox, error) {
			if requestid.FromContext(ctx) != "create-request-206" {
				t.Fatal("launch lost request ID")
			}
			if fail {
				cancel()
				return nil, errors.New("launch failed")
			}
			return launch(ctx, req, spec, ids, restore)
		}
		_, err := m.Create(ctx, testCreateRequest())
		if (err != nil) != fail || b.prepareID != "create-request-206" {
			t.Fatalf("create error %v, boot ID %q", err, b.prepareID)
		}
		if fail {
			if b.releaseID != "create-request-206" || b.releaseErr != nil {
				t.Fatalf("rollback ID %q, context error %v", b.releaseID, b.releaseErr)
			}
		} else {
			entry, err := m.loadSandboxEntry("sandbox-a")
			if err != nil || entry.sourceRequestID != "create-request-206" {
				t.Fatal("sandbox tracking lost source request ID")
			}
		}
	}
}

func TestSandboxExitCleanupRetainsCreationRequestID(t *testing.T) {
	m, store, _ := newLifecycleTestManager(t, readyBoot())
	var gotID string
	entry := seedRuntime(t, m, store, func(ctx context.Context) error {
		gotID = requestid.FromContext(ctx)
		return nil
	})
	entry.sourceRequestID = "creation-request-206"
	m.handleSandboxExit("sandbox-a", entry)
	if gotID != "creation-request-206" {
		t.Fatalf("async cleanup ID = %q", gotID)
	}
}
