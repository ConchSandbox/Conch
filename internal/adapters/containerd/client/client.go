package containerdclient

import (
	"context"
	"fmt"

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

func (c *Client) WithNamespace(ctx context.Context) (context.Context, error) {
	if c == nil || c.Client == nil {
		return nil, fmt.Errorf("containerd client is nil")
	}
	return NewNamespaceContext(ctx), nil
}

// Close closes the containerd client connection.
func (c *Client) Close() error {
	if c == nil || c.Client == nil {
		return nil
	}
	return c.Client.Close()
}
