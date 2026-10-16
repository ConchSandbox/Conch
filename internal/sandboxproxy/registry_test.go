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

func TestRegistrySlotLifecycle(t *testing.T) {
	r := NewRegistry()
	id := uuid.NewString()
	first := r.Insert(id)
	if first == nil {
		t.Fatal("insert returned no bootstrap context")
	}
	if _, ok := r.Lookup(id); ok {
		t.Fatal("unready guest exposed through registry")
	}
	if !r.Unpublished(id) {
		t.Fatal("bootstrapping slot not reported as unpublished")
	}
	if err := r.Publish(id, "127.0.0.1", ""); err != nil {
		t.Fatal(err)
	}
	old, _ := r.Lookup(id)
	if r.Insert(id) == nil {
		t.Fatal("replacement insert failed")
	}
	if old.lifetime.Err() == nil {
		t.Fatal("slot replacement did not invalidate previous connections")
	}
	if _, ok := r.Lookup(id); ok {
		t.Fatal("replacement slot exposed before publication")
	}
	if err := r.Publish(id, "127.0.0.2", ""); err != nil {
		t.Fatal(err)
	}
	current, ok := r.Lookup(id)
	if !ok || current.IP != "127.0.0.2" {
		t.Fatalf("current route = %+v, %v", current, ok)
	}
	if err := r.Publish(id, "127.0.0.3", ""); err == nil {
		t.Fatal("changed runtime IP without replacing connections")
	}
	r.Release(id)
	if _, ok := r.Lookup(id); ok {
		t.Fatal("released slot still resolvable")
	}
	if r.Unpublished(id) {
		t.Fatal("released slot still reported as unpublished")
	}
	if current.lifetime.Err() == nil {
		t.Fatal("release did not invalidate current connections")
	}
	if err := r.Publish(id, "127.0.0.2", ""); err == nil {
		t.Fatal("released slot resurrected by publication")
	}
	r.Release(id)
	third := r.Insert(id)
	if third == nil || third.Err() != nil {
		t.Fatal("cannot reinsert after release")
	}
}

func TestRegistryPublishRejectsInvalidInput(t *testing.T) {
	r := NewRegistry()
	defer r.Close()
	id := uuid.NewString()
	if err := r.Publish(id, "127.0.0.1", ""); err == nil {
		t.Fatal("published a sandbox without a slot")
	}
	ctx := r.Insert(id)
	if ctx == nil {
		t.Fatal("insert failed")
	}
	for _, test := range []struct{ name, ip, mask string }{
		{"unspecified ip", "0.0.0.0", ""},
		{"multicast ip", "224.0.0.1", ""},
		{"invalid ip", "not-an-ip", ""},
		{"zoned ip", "fe80::1%eth0", ""},
		{"masked host with space", "127.0.0.1", "app .internal"},
		{"masked host with slash", "127.0.0.1", "app/internal"},
	} {
		if err := r.Publish(id, test.ip, test.mask); err == nil {
			t.Errorf("%s: publish accepted %q %q", test.name, test.ip, test.mask)
		}
	}
	if err := r.Publish(id, "127.0.0.1", "  app-${PORT}.internal  "); err != nil {
		t.Fatalf("trimmed mask publication rejected: %v", err)
	}
	route, ok := r.Lookup(id)
	if !ok || route.MaskRequestHost != "app-${PORT}.internal" {
		t.Fatalf("mask not trimmed on published route: %+v, %v", route, ok)
	}
}

// TestRouteForObservesPublicationAtomically covers the waiter's read pattern:
// a slot first read without its IP, then published, must read back as a route
// from one lock hold — never as a vanished slot that would answer 502.
func TestRouteForObservesPublicationAtomically(t *testing.T) {
	r := NewRegistry()
	defer r.Close()
	id := uuid.NewString()
	if _, state := r.routeFor(id); state != routeAbsent {
		t.Fatalf("absent slot read as state %d", state)
	}
	if r.Insert(id) == nil {
		t.Fatal("insert failed")
	}
	if route, state := r.routeFor(id); state != routeUnpublished || route.IP != "" {
		t.Fatalf("bootstrapping slot read as state %d route %+v", state, route)
	}
	if err := r.Publish(id, "127.0.0.1", ""); err != nil {
		t.Fatal(err)
	}
	route, state := r.routeFor(id)
	if state != routePublished || route.IP != "127.0.0.1" {
		t.Fatalf("published slot read as state %d route %+v", state, route)
	}
	r.Release(id)
	if _, state := r.routeFor(id); state != routeAbsent {
		t.Fatalf("released slot read as state %d", state)
	}
}

func TestRegistryCloseCancelsAndRejectsNewSlots(t *testing.T) {
	r := NewRegistry()
	id := uuid.NewString()
	if r.Insert(id) == nil {
		t.Fatal("insert failed")
	}
	if err := r.Publish(id, "127.0.0.1", ""); err != nil {
		t.Fatal(err)
	}
	route, _ := r.Lookup(id)
	r.Close()
	r.Close()
	if route.lifetime.Err() == nil {
		t.Fatal("shutdown did not cancel active route")
	}
	if ctx := r.Insert(id); ctx != nil {
		t.Fatal("closed registry allocated a new slot")
	}
	if err := r.Publish(id, "127.0.0.1", ""); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("closed publication = %v", err)
	}
	w := httptest.NewRecorder()
	NewHandler(r, nil).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/proxy/health", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed proxy status = %d", w.Code)
	}
}

func TestInsertProtectsUnpublishedBootstrap(t *testing.T) {
	r := NewRegistry()
	id := uuid.NewString()
	first := r.Insert(id)
	if first.Err() != nil {
		t.Fatal("unpublished runtime has no live bootstrap context")
	}
	second := r.Insert(id)
	if first.Err() == nil {
		t.Fatal("replacement did not cancel old bootstrap")
	}
	if second.Err() != nil {
		t.Fatal("replacement bootstrap is unavailable")
	}
	r.Release(id)
	if second.Err() == nil {
		t.Fatal("resource release did not cancel unpublished bootstrap")
	}
	third := r.Insert(id)
	r.Close()
	if third.Err() == nil {
		t.Fatal("Node shutdown did not cancel unpublished bootstrap")
	}
}

func TestProxyRequestContextCancelsSynchronouslyWithRelease(t *testing.T) {
	r := NewRegistry()
	defer r.Close()
	id := uuid.NewString()
	if r.Insert(id) == nil {
		t.Fatal("insert failed")
	}
	if err := r.Publish(id, "127.0.0.1", ""); err != nil {
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
	r.Release(id)
	// No wait or scheduling yield: removal must return only after the
	// request's cancellation is observable, before the guest IP is reused.
	if !errors.Is(proxied.Err(), context.Canceled) {
		t.Fatalf("route release did not synchronously cancel proxy: %v", proxied.Err())
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
	if r.Insert(id) == nil {
		t.Fatal("insert failed")
	}
	if err := r.Publish(id, "127.0.0.1", ""); err != nil {
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
