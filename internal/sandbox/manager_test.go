package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/template"
)

func TestReserveSandboxEntryDoesNotBlockDifferentSandbox(t *testing.T) {
	m := &Manager{}

	key, entry, err := m.reserveSandboxEntry("ns", "sandbox-a")
	if err != nil {
		t.Fatalf("reserveSandboxEntry() error = %v", err)
	}
	defer m.sandboxes.CompareAndDelete(key, entry)
	defer entry.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		otherKey, otherEntry, err := m.reserveSandboxEntry("ns", "sandbox-b")
		if err == nil {
			m.sandboxes.CompareAndDelete(otherKey, otherEntry)
			otherEntry.mu.Unlock()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reserveSandboxEntry() for different sandbox error = %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("reserveSandboxEntry() for different sandbox blocked behind another sandbox entry")
	}
}

func TestHandleSandboxExitCleansSuspendedSandbox(t *testing.T) {
	templateAdapter := &recordingTemplate{}
	m := &Manager{
		template:     templateAdapter,
		cidAllocator: NewCIDAllocatorInDir(t.TempDir()),
	}
	sbx := &Sandbox{
		cleanup:   NewCleanup(),
		namespace: "ns",
		sandboxID: "sandbox-a",
	}
	entry := &sandboxEntry{state: sandboxSuspended, sbx: sbx}
	mapKey := sandboxMapKey("ns", "sandbox-a")
	m.sandboxes.Store(mapKey, entry)

	m.handleSandboxExit(mapKey, entry, "sandbox-a", sbx)

	if _, ok := m.sandboxes.Load(mapKey); ok {
		t.Fatal("suspended sandbox entry remains after VMM exit")
	}
	if len(templateAdapter.released) != 1 || templateAdapter.released[0].SandboxID != "sandbox-a" {
		t.Fatalf("released boot layouts = %#v", templateAdapter.released)
	}
}

func TestCommitCheckpointResumesOnlyAfterCommit(t *testing.T) {
	var events []string
	runtime := &recordingCheckpointRuntime{
		capturePath: t.TempDir(),
		events:      &events,
	}
	templateAdapter := &recordingTemplate{events: &events}
	m := &Manager{template: templateAdapter, requestTimeout: time.Second}

	commit, resumeFailed, err := m.commitCheckpoint(
		context.Background(),
		context.Background(),
		runtime,
		true,
		template.CommitSandboxBootRequest{
			Namespace:  "ns",
			SandboxID:  "sandbox-a",
			TemplateID: "tmpl_a",
		},
	)
	if err != nil {
		t.Fatalf("commitCheckpoint() error = %v", err)
	}
	if resumeFailed {
		t.Fatal("commitCheckpoint() reported resume failure")
	}
	if got, want := strings.Join(events, ","), "capture,commit,resume"; got != want {
		t.Fatalf("checkpoint event order = %q, want %q", got, want)
	}
	if commit.RootfsKey != "rootfs" {
		t.Fatalf("checkpoint commit = %#v", commit)
	}
}

type recordingCheckpointRuntime struct {
	capturePath string
	events      *[]string
}

func (r *recordingCheckpointRuntime) captureCheckpoint(context.Context, bool) (string, error) {
	*r.events = append(*r.events, "capture")
	return r.capturePath, nil
}

func (r *recordingCheckpointRuntime) Resume(context.Context) error {
	*r.events = append(*r.events, "resume")
	return nil
}

type recordingTemplate struct {
	released []template.ReleaseSandboxBootRequest
	events   *[]string
}

func (r *recordingTemplate) PrepareSandboxBoot(context.Context, template.PrepareSandboxBootRequest) (template.PreparedSandboxBoot, error) {
	return template.PreparedSandboxBoot{}, nil
}

func (r *recordingTemplate) ReleaseSandboxBoot(_ context.Context, req template.ReleaseSandboxBootRequest) error {
	r.released = append(r.released, req)
	return nil
}

func (r *recordingTemplate) CommitSandboxBoot(context.Context, template.CommitSandboxBootRequest) (template.SandboxBootCommit, error) {
	if r.events != nil {
		*r.events = append(*r.events, "commit")
	}
	return template.SandboxBootCommit{RootfsKey: "rootfs", MemKey: "mem", VMKey: "vm"}, nil
}

func TestReserveSandboxEntrySerializesSameSandbox(t *testing.T) {
	m := &Manager{}

	key, entry, err := m.reserveSandboxEntry("ns", "sandbox-a")
	if err != nil {
		t.Fatalf("reserveSandboxEntry() error = %v", err)
	}
	defer m.sandboxes.CompareAndDelete(key, entry)

	done := make(chan error, 1)
	go func() {
		_, _, err := m.reserveSandboxEntry("ns", "sandbox-a")
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("same sandbox reserve completed while entry lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	entry.state = sandboxReady
	entry.mu.Unlock()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("same sandbox reserve succeeded while an entry already existed")
		}
		if !strings.Contains(err.Error(), "ready") {
			t.Fatalf("same sandbox reserve error = %v, want ready state", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("same sandbox reserve did not unblock after entry lock was released")
	}
}

func TestLifecycleOperationsRejectCreatingSandbox(t *testing.T) {
	m := &Manager{}

	key, entry, err := m.reserveSandboxEntry("ns", "sandbox-a")
	if err != nil {
		t.Fatalf("reserveSandboxEntry() error = %v", err)
	}
	entry.mu.Unlock()
	defer m.sandboxes.CompareAndDelete(key, entry)

	err = m.Delete(SandboxDeleteRequest{Namespace: "ns", SandboxId: "sandbox-a"})
	if err == nil {
		t.Fatal("Delete() succeeded for creating sandbox")
	}
	if !strings.Contains(err.Error(), "creating") {
		t.Fatalf("Delete() error = %v, want creating state", err)
	}

	_, err = m.Checkpoint(SandboxCheckpointRequest{Namespace: "ns", SandboxId: "sandbox-a"})
	if err == nil {
		t.Fatal("Checkpoint() succeeded for creating sandbox")
	}
	if !strings.Contains(err.Error(), "creating") {
		t.Fatalf("Checkpoint() error = %v, want creating state", err)
	}
}
