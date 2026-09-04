package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/conchruntime"
	conchsandbox "github.com/openeuler/Conch/internal/sandbox"
)

// tempSocket returns a short socket path; unix paths cap at ~108 bytes,
// which t.TempDir() names can exceed.
func tempSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "conchd")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// newTestDaemon mirrors what New builds, minus the parts these tests do not use.
func newTestDaemon(store conchsandbox.Store) *Daemon {
	mux := http.NewServeMux()
	return &Daemon{
		router:         mux,
		httpServer:     newHTTPServer(mux),
		sandboxStore:   store,
		runtimeService: &conchruntime.Service{},
	}
}

// A signal handled before Start reached Serve used to leave Start blocked in
// Serve forever, on a listener nobody was left to close.
func TestStartAfterShutdownDoesNotServe(t *testing.T) {
	socket := tempSocket(t)
	s := newTestDaemon(nil)

	s.Shutdown()

	done := make(chan error, 1)
	go func() { done <- s.Start(socket) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start after Shutdown returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start blocked in Serve although Shutdown had already run")
	}

	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket %s was left behind: %v", socket, err)
	}
}

// slowListStore keeps teardown running long enough to be observed. Only List
// is exercised; the embedded nil Store satisfies the rest of the interface.
type slowListStore struct {
	conchsandbox.Store
	started chan struct{}
	done    chan struct{}
}

func (s *slowListStore) List(context.Context, conchsandbox.Filter) ([]conchsandbox.Record, error) {
	close(s.started)
	time.Sleep(300 * time.Millisecond)
	close(s.done)
	return nil, nil
}

// When Shutdown gets there first, Start returns as soon as Serve refuses. The
// caller must still wait for that in-flight cleanup.
func TestStartWaitsForInFlightCleanup(t *testing.T) {
	socket := tempSocket(t)
	store := &slowListStore{started: make(chan struct{}), done: make(chan struct{})}
	s := newTestDaemon(store)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		s.Shutdown()
	}()

	<-store.started

	if err := s.Start(socket); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// This is what main's deferred Cleanup does after Start returns.
	s.Cleanup()

	select {
	case <-store.done:
	default:
		t.Fatal("Cleanup returned while the in-flight shutdown was still tearing down")
	}
	<-shutdownDone
}

// Whichever of Start and Shutdown runs first, the daemon must end up stopped
// rather than serving on a listener nothing will ever close.
func TestShutdownRacingStartAlwaysStops(t *testing.T) {
	for i := 0; i < 50; i++ {
		socket := tempSocket(t)
		s := newTestDaemon(nil)

		done := make(chan error, 1)
		go func() { done <- s.Start(socket) }()
		s.Shutdown()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("iteration %d: Start returned error: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("iteration %d: Start never returned after Shutdown", i)
		}

		if _, err := os.Stat(socket); !os.IsNotExist(err) {
			t.Fatalf("iteration %d: socket %s was left behind: %v", i, socket, err)
		}
	}
}
