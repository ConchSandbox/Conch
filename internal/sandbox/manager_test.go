package sandbox

import (
	"strings"
	"testing"
	"time"
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

	_, err = m.Pause(SandboxPauseRequest{Namespace: "ns", SandboxId: "sandbox-a"})
	if err == nil {
		t.Fatal("Pause() succeeded for creating sandbox")
	}
	if !strings.Contains(err.Error(), "creating") {
		t.Fatalf("Pause() error = %v, want creating state", err)
	}
}
