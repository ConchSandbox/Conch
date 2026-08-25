package containerdclient

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/plugin"
)

const Namespace = "conch"

func NewNamespaceContext(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, Namespace)
}

// Client wraps containerd client connection and provides namespace management
type Client struct {
	*containerd.Client // embedded containerd client
}

// NewInMemory creates a client backed by containerd services in the current
// process. It is intended for conchd's embedded containerd host path.
func NewInMemory(ic *plugin.InitContext, opts ...containerd.Opt) (*Client, error) {
	opts = append(opts, containerd.WithInMemoryServices(ic))
	cli, err := containerd.New("", opts...)
	if err != nil {
		return nil, err
	}
	return &Client{Client: cli}, nil
}

func RuntimeLeaseID() string {
	return "conch.runtime"
}

func (c *Client) WithNamespace(ctx context.Context) (context.Context, error) {
	if c == nil || c.Client == nil {
		return nil, fmt.Errorf("containerd client is nil")
	}
	return NewNamespaceContext(ctx), nil
}

func (c *Client) WithRuntimeLease(ctx context.Context, leaseID string) (context.Context, string, error) {
	if c == nil || c.Client == nil {
		return nil, "", fmt.Errorf("containerd client is nil")
	}
	if leaseID == "" {
		leaseID = RuntimeLeaseID()
	}
	namespaceCtx := NewNamespaceContext(ctx)
	if err := c.ensureLease(namespaceCtx, leaseID, map[string]string{
		"io.conch.lease.kind": "runtime",
	}); err != nil {
		return nil, "", err
	}
	return leases.WithLease(namespaceCtx, leaseID), leaseID, nil
}

func (c *Client) ensureLease(ctx context.Context, leaseID string, labels map[string]string) error {
	items, err := c.LeasesService().List(ctx)
	if err != nil {
		return fmt.Errorf("list leases: %w", err)
	}
	for _, item := range items {
		if item.ID == leaseID {
			return nil
		}
	}
	_, err = c.LeasesService().Create(ctx, leases.WithID(leaseID), leases.WithLabels(labels))
	if err != nil {
		// The runtime lease is a process-wide singleton shared by every sandbox;
		// concurrent cold starts can race to create it. Treat an already-existing
		// lease as success (idempotent) instead of failing the sandbox.
		if errdefs.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create runtime lease %s: %w", leaseID, err)
	}
	return nil
}

// Close closes the containerd client connection.
func (c *Client) Close() error {
	if c == nil || c.Client == nil {
		return nil
	}
	return c.Client.Close()
}
