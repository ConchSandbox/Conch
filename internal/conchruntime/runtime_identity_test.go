package conchruntime

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/openeuler/Conch/internal/sandbox"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

func TestDelayedExitDoesNotRetireReusedSandboxID(t *testing.T) {
	ctx := context.Background()
	store := newMemorySandboxStore()
	templateID := digest.FromString("runtime-identity-template").String()
	ops := &fakeSandboxOps{store: store}
	svc := New(ops, nil)
	svc.Store = store
	setFakeTemplate(svc, testTemplateName, templateID, conchtemplate.BootModeCold)
	var err error
	svc.Capacity, err = NewCapacity(CapacityLimits{MaxSandboxes: 1, MaxCPUs: 2, MaxMemoryMB: 512})
	if err != nil {
		t.Fatal(err)
	}
	opts := SandboxCreateOptions{SandboxID: "reused-id", TemplateName: testTemplateName, VCPUNum: 2, VCPUMax: 2, RamMB: 512}
	if _, err := svc.CreateSandbox(ctx, opts); err != nil {
		t.Fatal(err)
	}
	old, err := svc.GetSandbox(ctx, opts.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveSandbox(ctx, opts.SandboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSandbox(ctx, opts); err != nil {
		t.Fatal(err)
	}
	replacement, err := svc.GetSandbox(ctx, opts.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if old.RuntimeID == "" || replacement.RuntimeID == "" || old.RuntimeID == replacement.RuntimeID || ops.req.RuntimeID != replacement.RuntimeID {
		t.Fatalf("runtime identity was reused: old=%+v new=%+v", old, replacement)
	}
	// This is the original observer arriving after DELETE + native ID reuse.
	// Manager has no record for the exited runtime, so the stale exit is a
	// no-op and must not touch the replacement or refund its reservation.
	svc.HandleSandboxUnexpectedExit(old.ID, old.RuntimeID, nil)
	after, err := svc.GetSandbox(ctx, replacement.ID)
	if err != nil || !reflect.DeepEqual(after, replacement) {
		t.Fatalf("stale exit changed replacement: %+v err=%v", after, err)
	}
	if err := svc.Capacity.reserve("extra", 2, 512); !errors.Is(err, sandbox.ErrResourceExhausted) {
		t.Fatalf("stale exit refunded replacement capacity: %v", err)
	}
	// The replacement's own exit releases its reservation through the record
	// Manager persisted for it. The record itself stays behind as Manager's
	// UNKNOWN owner; a fresh ID can consume the freed capacity.
	svc.HandleSandboxUnexpectedExit(replacement.ID, replacement.RuntimeID, nil)
	if _, err := svc.GetSandbox(ctx, replacement.ID); err != nil {
		t.Fatalf("unexpected exit removed a Manager-owned record: %v", err)
	}
	if err := svc.Capacity.reserve("extra", 2, 512); err != nil {
		t.Fatalf("current exit did not release capacity: %v", err)
	}
}
