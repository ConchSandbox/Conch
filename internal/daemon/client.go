package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/containerd/containerd/leases"
	"golang.org/x/sys/unix"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
)

// session represents a namespace session with context and cancellation
type session struct {
	ctx    context.Context
	cancel context.CancelFunc
	lease  leases.Lease
}

// Client wraps containerd client connection and provides namespace management
type Client struct {
	*containerd.Client                     // embedded containerd client
	sessions           map[string]*session // namespace -> session mapping
	mu                 sync.RWMutex        // protects sessions
}

// New creates and connects to containerd daemon
func New(address string, opts ...containerd.ClientOpt) (*Client, error) {
	cli, err := initClient(address, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{
		Client:   cli,
		sessions: make(map[string]*session),
	}, nil
}

// GetNamespaceContext returns an existing namespace context
// Returns (context, bool) - (nil, false) if namespace not found
func (c *Client) GetNamespaceContext(namespace string) (context.Context, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.sessions[namespace]
	if !ok {
		return nil, false
	}
	return s.ctx, true
}

// WithNamespace returns a namespace context, creating a new session if needed
// Uses double-check locking to avoid duplicate session creation
func (c *Client) WithNamespace(ctx context.Context, namespace string) (context.Context, error) {
	ns := namespace
	if ns == "" {
		ns = c.Client.DefaultNamespace()
	}

	c.mu.RLock()
	if s, ok := c.sessions[ns]; ok {
		c.mu.RUnlock()
		return s.ctx, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.sessions[ns]; ok {
		return s.ctx, nil
	}

	namespaceCtx := namespaces.WithNamespace(context.Background(), ns)
	lease, err := c.LeasesService().Create(namespaceCtx, leases.WithRandomID())
	if err != nil {
		return nil, fmt.Errorf("create lease for namespace %s: %w", ns, err)
	}

	sessionCtx, cancel := context.WithCancel(leases.WithLease(namespaceCtx, lease.ID))
	c.sessions[ns] = &session{ctx: sessionCtx, cancel: cancel, lease: lease}
	return sessionCtx, nil
}

// Close closes the connection and cleans up all sessions
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var errs []error
	for k, v := range c.sessions {
		namespaceCtx := namespaces.WithNamespace(context.Background(), k)
		if err := c.LeasesService().Delete(namespaceCtx, v.lease); err != nil {
			errs = append(errs, fmt.Errorf("delete lease %s in namespace %s: %w", v.lease.ID, k, err))
		}
		v.cancel()
		delete(c.sessions, k)
	}
	if err := c.Client.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// DefaultNamespace returns the default namespace
func (c *Client) DefaultNamespace() string {
	return c.Client.DefaultNamespace()
}

// checkSocket checks if the containerd socket file is accessible
func checkSocket(s string) error {
	return unix.Faccessat(-1, s, unix.R_OK|unix.W_OK, unix.AT_EACCESS)
}

// initClient initializes containerd client connection
func initClient(address string, opts ...containerd.ClientOpt) (*containerd.Client, error) {
	address = strings.TrimPrefix(address, "unix://")
	if err := checkSocket(address); err != nil {
		return nil, fmt.Errorf("access containerd socket %q: %w", address, err)
	}
	cli, err := containerd.New(address, opts...)
	if err != nil {
		return nil, err
	}
	return cli, nil
}
