package containerdclient

import (
	"context"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

func TestWithNamespaceUsesConch(t *testing.T) {
	ctx, err := (*Client)(nil).WithNamespace(context.Background())
	if err == nil {
		t.Fatalf("WithNamespace() error = nil, want error")
	}
	if ctx != nil {
		t.Fatalf("WithNamespace() ctx = %v, want nil", ctx)
	}

	nsCtx, err := (&Client{Client: &containerd.Client{}}).WithNamespace(context.Background())
	if err != nil {
		t.Fatalf("WithNamespace() error = %v", err)
	}
	ns, ok := namespaces.Namespace(nsCtx)
	if !ok || ns != Namespace {
		t.Fatalf("namespace = %q ok=%v, want %s true", ns, ok, Namespace)
	}
}
