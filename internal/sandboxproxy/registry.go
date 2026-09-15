// Package sandboxproxy routes E2B data-plane traffic to this Node's guests.
package sandboxproxy

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
)

var ErrStaleGeneration = errors.New("sandbox runtime generation is no longer current")
var ErrRegistryClosed = errors.New("sandbox proxy registry is closed")

// Route is a published runtime address, never a client-supplied upstream URL.
// MaskRequestHost optionally rewrites the proxied request's Host header.
type Route struct {
	SandboxID        string
	IP               string
	Generation       uint64
	MaskRequestHost  string
	lifetime         context.Context
}

type entry struct {
	route  Route
	cancel context.CancelFunc
}

// Registry invalidates old requests as well as protecting new routes from old
// asynchronous cleanup. Lifecycle callers retain Begin's generation until
// cleanup and pass that same value to Publish and Remove.
type Registry struct {
	mu      sync.RWMutex
	next    uint64
	entries map[string]entry
	closed  bool
}

func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]entry)}
}

// Begin reserves a fresh generation, initially invisible to proxy lookup.
// Replacing an old generation cancels every request attached to that runtime.
// It returns zero after Close; zero can never be published.
func (r *Registry) Begin(sandboxID string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0
	}
	if previous, ok := r.entries[sandboxID]; ok {
		previous.cancel()
	}
	r.next++
	ctx, cancel := context.WithCancel(context.Background())
	r.entries[sandboxID] = entry{
		route:  Route{SandboxID: sandboxID, Generation: r.next, lifetime: ctx},
		cancel: cancel,
	}
	return r.next
}

// Publish makes a generation proxyable only after envd health and init succeed.
func (r *Registry) Publish(sandboxID string, generation uint64, ip string) error {
	return r.PublishWithHostMask(sandboxID, generation, ip, "")
}

// PublishWithHostMask additionally records an optional masked Host value the
// proxy substitutes into proxied requests. An empty mask preserves the
// original Host.
func (r *Registry) PublishWithHostMask(sandboxID string, generation uint64, ip, maskRequestHost string) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil || addr.Zone() != "" || addr.IsUnspecified() || addr.IsMulticast() {
		return fmt.Errorf("invalid sandbox interaction IP %q", ip)
	}
	maskRequestHost = strings.TrimSpace(maskRequestHost)
	if maskRequestHost != "" && strings.ContainsAny(maskRequestHost, " \t\r\n/") {
		return fmt.Errorf("invalid masked request host %q", maskRequestHost)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRegistryClosed
	}
	current, ok := r.entries[sandboxID]
	if !ok || current.route.Generation != generation {
		return ErrStaleGeneration
	}
	if current.route.IP != "" && current.route.IP != addr.String() {
		return errors.New("changing runtime IP requires a new generation")
	}
	current.route.IP = addr.String()
	current.route.MaskRequestHost = maskRequestHost
	r.entries[sandboxID] = current
	return nil
}

// Remove also invalidates an unpublished generation. Stale cleanup is a no-op.
func (r *Registry) Remove(sandboxID string, generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.entries[sandboxID]
	if !ok || current.route.Generation != generation {
		return false
	}
	current.cancel()
	delete(r.entries, sandboxID)
	return true
}

func (r *Registry) Lookup(sandboxID string) (Route, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	current, ok := r.entries[sandboxID]
	return current.route, ok && current.route.IP != ""
}

// CurrentGeneration includes a generation whose guest is still bootstrapping.
// Callers without an operation's saved generation must hold the lifecycle lock
// while taking this snapshot and removing the corresponding runtime.
func (r *Registry) CurrentGeneration(sandboxID string) (uint64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	current, ok := r.entries[sandboxID]
	return current.route.Generation, ok
}

// GenerationContext binds guest bootstrap work to a runtime before its route
// is published. Removal, replacement or Node shutdown cancels the context, so
// an old bootstrap cannot keep using an interaction IP after resource release.
func (r *Registry) GenerationContext(sandboxID string, generation uint64) (context.Context, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	current, ok := r.entries[sandboxID]
	if !ok || current.route.Generation != generation {
		return nil, false
	}
	return current.route.lifetime, true
}

// Close invalidates all HTTP streams and upgraded WebSocket connections before
// the Node waits for graceful HTTP shutdown. A closed registry cannot reopen.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for _, entry := range r.entries {
		entry.cancel()
	}
	clear(r.entries)
}

func (r *Registry) isClosed() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.closed
}
