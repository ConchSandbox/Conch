package containerdclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/containerd/v2/core/leases"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/plugin"
)

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

func RuntimeLeaseID(namespace string) string {
	ns := strings.TrimSpace(namespace)
	if ns == "" {
		ns = "default"
	}
	return "conch.runtime." + ns
}

func (c *Client) WithNamespace(ctx context.Context, namespace string) (context.Context, error) {
	ns := strings.TrimSpace(namespace)
	if ns == "" {
		if c == nil || c.Client == nil {
			return nil, fmt.Errorf("containerd client is nil")
		}
		ns = c.Client.DefaultNamespace()
	}
	if ns == "" {
		ns = "default"
	}
	return namespaces.WithNamespace(ctx, ns), nil
}

func (c *Client) WithRuntimeLease(ctx context.Context, namespace, leaseID string) (context.Context, string, error) {
	if c == nil || c.Client == nil {
		return nil, "", fmt.Errorf("containerd client is nil")
	}
	ns := namespace
	if ns == "" {
		ns = c.Client.DefaultNamespace()
	}
	if ns == "" {
		ns = "default"
	}
	if leaseID == "" {
		leaseID = RuntimeLeaseID(ns)
	}
	namespaceCtx := namespaces.WithNamespace(ctx, ns)
	if err := c.ensureLease(namespaceCtx, leaseID, map[string]string{
		"io.conch.lease.kind":      "runtime",
		"io.conch.lease.namespace": ns,
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

// DefaultNamespace returns the default namespace
func (c *Client) DefaultNamespace() string {
	return c.Client.DefaultNamespace()
}
