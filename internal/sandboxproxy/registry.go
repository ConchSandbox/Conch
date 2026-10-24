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

var ErrRegistryClosed = errors.New("sandbox proxy registry is closed")

// Route is a published runtime address, never a client-supplied upstream URL.
// MaskRequestHost optionally rewrites the proxied request's Host header.
type Route struct {
	IP              string
	MaskRequestHost string
	lifetime        context.Context
}

type entry struct {
	route  Route
	cancel context.CancelFunc
}

// Registry maps each sandbox to a single route slot. The sandbox's lifecycle
// lock serializes Insert/Publish/Release; the registry's own mutex protects
// the proxy hot path reading published routes.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]entry
	closed  bool
}

func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]entry)}
}

// Insert reserves an unpublished route slot, initially invisible to proxy
// lookup, and returns its lifetime context. Replacing an existing slot cancels
// every request attached to that runtime. It returns nil after Close.
func (r *Registry) Insert(sandboxID string) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	if previous, ok := r.entries[sandboxID]; ok {
		previous.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.entries[sandboxID] = entry{
		route:  Route{lifetime: ctx},
		cancel: cancel,
	}
	return ctx
}

// Publish makes a route proxyable once envd health and init succeed. An empty
// mask preserves the original Host.
func (r *Registry) Publish(sandboxID, ip, maskRequestHost string) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil || addr.Zone() != "" || addr.IsUnspecified() || addr.IsMulticast() {
		return fmt.Errorf("invalid sandbox interaction IP %q", ip)
	}
	maskRequestHost, err = ValidateMaskRequestHost(maskRequestHost)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrRegistryClosed
	}
	current, ok := r.entries[sandboxID]
	if !ok {
		return fmt.Errorf("sandbox %s has no route slot", sandboxID)
	}
	if current.route.IP != "" && current.route.IP != addr.String() {
		return errors.New("changing runtime IP requires a new route slot")
	}
	current.route.IP = addr.String()
	current.route.MaskRequestHost = maskRequestHost
	r.entries[sandboxID] = current
	return nil
}

// Unpublish withdraws a published route's IP while keeping the slot: requests
// attached to the route stop, and proxy waiters keep waiting for the slot to
// publish again. An absent or unpublished slot is a no-op.
func (r *Registry) Unpublish(sandboxID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.entries[sandboxID]
	if !ok || current.route.IP == "" {
		return
	}
	current.cancel()
	ctx, cancel := context.WithCancel(context.Background())
	current.route.IP = ""
	current.route.lifetime = ctx
	current.cancel = cancel
	r.entries[sandboxID] = current
}

// Release cancels every request attached to the route and removes the slot
// before the sandbox's network resources return to the pool. Releasing an
// absent slot is a no-op.
func (r *Registry) Release(sandboxID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.entries[sandboxID]; ok {
		current.cancel()
		delete(r.entries, sandboxID)
	}
}

// routeState is a slot's visibility to the proxy, read atomically with the
// route it describes.
type routeState int

const (
	routeAbsent routeState = iota
	routeUnpublished
	routePublished
)

// routeFor reads a slot and its state under one lock hold, so a publication
// landing between two separate lookups cannot read as a vanished slot.
func (r *Registry) routeFor(sandboxID string) (Route, routeState) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	current, ok := r.entries[sandboxID]
	switch {
	case !ok:
		return Route{}, routeAbsent
	case current.route.IP == "":
		return current.route, routeUnpublished
	default:
		return current.route, routePublished
	}
}

func (r *Registry) Lookup(sandboxID string) (Route, bool) {
	route, state := r.routeFor(sandboxID)
	return route, state == routePublished
}

func ValidateMaskRequestHost(maskRequestHost string) (string, error) {
	maskRequestHost = strings.TrimSpace(maskRequestHost)
	if maskRequestHost != "" && strings.ContainsAny(maskRequestHost, " \t\r\n/") {
		return "", fmt.Errorf("invalid masked request host %q", maskRequestHost)
	}
	return maskRequestHost, nil
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
