package sandboxproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRegistryGenerationIsolation(t *testing.T) {
	r := NewRegistry()
	id := uuid.NewString()
	first := r.Begin(id)
	if _, ok := r.Lookup(id); ok {
		t.Fatal("unready guest exposed through registry")
	}
	if err := r.Publish(id, first, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	old, _ := r.Lookup(id)
	second := r.Begin(id)
	if old.lifetime.Err() == nil {
		t.Fatal("new generation did not invalidate previous connections")
	}
	if err := r.Publish(id, first, "127.0.0.2"); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale publication = %v", err)
	}
	if err := r.Publish(id, second, "127.0.0.3"); err != nil {
		t.Fatal(err)
	}
	if r.Remove(id, first) {
		t.Fatal("stale cleanup removed current generation")
	}
	current, ok := r.Lookup(id)
	if !ok || current.IP != "127.0.0.3" || current.Generation != second {
		t.Fatalf("current route = %+v, %v", current, ok)
	}
	if err := r.Publish(id, second, "127.0.0.4"); err == nil {
		t.Fatal("changed generation IP without replacing connections")
	}
	if !r.Remove(id, second) || current.lifetime.Err() == nil {
		t.Fatal("cleanup did not invalidate current generation")
	}
	if err := r.Publish(id, second, "127.0.0.3"); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("removed generation resurrected: %v", err)
	}
	third := r.Begin(id)
	if !r.Remove(id, third) {
		t.Fatal("cannot remove bootstrapping generation")
	}
}

func TestRegistryCloseCancelsAndRejectsNewGenerations(t *testing.T) {
	r := NewRegistry()
	id := uuid.NewString()
	gen := r.Begin(id)
	if err := r.Publish(id, gen, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	route, _ := r.Lookup(id)
	r.Close()
	r.Close()
	if route.lifetime.Err() == nil {
		t.Fatal("shutdown did not cancel active route")
	}
	if gen := r.Begin(id); gen != 0 {
		t.Fatalf("closed registry allocated generation %d", gen)
	}
	if err := r.Publish(id, gen, "127.0.0.1"); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("closed publication = %v", err)
	}
	w := httptest.NewRecorder()
	NewHandler(r, nil).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/proxy/health", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed proxy status = %d", w.Code)
	}
}

func TestGenerationContextProtectsUnpublishedBootstrap(t *testing.T) {
	r := NewRegistry()
	id := uuid.NewString()
	first := r.Begin(id)
	ctx, ok := r.GenerationContext(id, first)
	if !ok || ctx.Err() != nil {
		t.Fatal("unpublished runtime has no live bootstrap context")
	}
	second := r.Begin(id)
	if ctx.Err() == nil {
		t.Fatal("replacement did not cancel old bootstrap")
	}
	if _, ok := r.GenerationContext(id, first); ok {
		t.Fatal("stale bootstrap acquired current runtime context")
	}
	current, ok := r.GenerationContext(id, second)
	if !ok || current.Err() != nil {
		t.Fatal("replacement bootstrap is unavailable")
	}
	r.Remove(id, second)
	if current.Err() == nil {
		t.Fatal("resource release did not cancel unpublished bootstrap")
	}
	if _, ok := r.GenerationContext(id, second); ok {
		t.Fatal("removed runtime still provides bootstrap context")
	}
	third := r.Begin(id)
	current, _ = r.GenerationContext(id, third)
	r.Close()
	if current.Err() == nil {
		t.Fatal("Node shutdown did not cancel unpublished bootstrap")
	}
}

func TestProxyRequestContextCancelsSynchronouslyWithGeneration(t *testing.T) {
	r := NewRegistry()
	defer r.Close()
	id := uuid.NewString()
	generation := r.Begin(id)
	if err := r.Publish(id, generation, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	route, _ := r.Lookup(id)
	deadline := time.Now().Add(time.Minute)
	request, cancelRequest := context.WithDeadline(t.Context(), deadline)
	defer cancelRequest()
	proxied, cancel := route.requestContext(request)
	defer cancel()
	if got, ok := proxied.Deadline(); !ok || !got.Equal(deadline) {
		t.Fatalf("request deadline lost: %v, %v", got, ok)
	}
	r.Remove(id, generation)
	// No wait or scheduling yield: removal must return only after the
	// request's cancellation is observable, before the guest IP is reused.
	if !errors.Is(proxied.Err(), context.Canceled) {
		t.Fatalf("generation removal did not synchronously cancel proxy: %v", proxied.Err())
	}
	late, cancelLate := route.requestContext(request)
	defer cancelLate()
	if !errors.Is(late.Err(), context.Canceled) {
		t.Fatalf("already invalidated route produced live request context: %v", late.Err())
	}
}

func TestProxyRequestContextKeepsClientCancellation(t *testing.T) {
	r := NewRegistry()
	defer r.Close()
	id := uuid.NewString()
	generation := r.Begin(id)
	if err := r.Publish(id, generation, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	route, _ := r.Lookup(id)
	request, cancelRequest := context.WithCancel(t.Context())
	proxied, cancel := route.requestContext(request)
	defer cancel()
	cancelRequest()
	select {
	case <-proxied.Done():
	case <-time.After(time.Second):
		t.Fatal("client disconnect did not cancel proxied request")
	}
	if route.lifetime.Err() != nil {
		t.Fatal("client disconnect invalidated the whole guest runtime")
	}
}
