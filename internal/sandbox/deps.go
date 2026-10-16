package sandbox

import (
	"context"
	"time"

	"github.com/openeuler/Conch/internal/envd"
)

// bootstrapTimeout bounds an E2B create or resume from the moment it takes the
// sandbox lock: the vsock wait and the envd handshake share it, and the proxy
// grants late requests the same budget to wait for the route (§5, §7 of the
// single-lock lifecycle design).
const bootstrapTimeout = 60 * time.Second

// RouteTable is the data-plane registry a Manager publishes E2B routes into.
// It is implemented by the sandbox proxy; nil disables the E2B data plane.
type RouteTable interface {
	// Insert reserves an unpublished route slot and returns its lifetime
	// context, canceling the slot it replaces. nil means the registry closed.
	Insert(sandboxID string) context.Context
	// Publish exposes a slot after envd health and init succeeded.
	Publish(sandboxID, ip, maskRequestHost string) error
	// Release cancels in-flight requests and removes the slot before the
	// interaction IP can return to the network pool.
	Release(sandboxID string)
}

// CapacityLedger bounds E2B sandboxes' CPU and memory. Reservations follow
// record ownership: they are released only when a death is finalized.
type CapacityLedger interface {
	Reserve(sandboxID string, cpus, memoryMB int64) error
	Release(sandboxID string)
}

// EnvdBootstrapper is the guest-side envd client; nil skips the E2B bootstrap.
type EnvdBootstrapper interface {
	WaitReady(ctx context.Context, ip string) error
	Init(ctx context.Context, ip string, options envd.InitOptions) (string, error)
}
