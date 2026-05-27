package containerdclient

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
)

func TestRuntimeLeaseIDIsNamespaceScoped(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		want      string
	}{
		{name: "default namespace", namespace: "", want: "conch.runtime.default"},
		{name: "trim namespace", namespace: " kube-system ", want: "conch.runtime.kube-system"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RuntimeLeaseID(tt.namespace); got != tt.want {
				t.Fatalf("RuntimeLeaseID(%q) = %q, want %q", tt.namespace, got, tt.want)
			}
		})
	}
}

func TestWithNamespaceUsesFallbacks(t *testing.T) {
	ctx, err := (*Client)(nil).WithNamespace(context.Background(), "")
	if err == nil {
		t.Fatalf("WithNamespace() error = nil, want error")
	}
	if ctx != nil {
		t.Fatalf("WithNamespace() ctx = %v, want nil", ctx)
	}

	nsCtx, err := (&Client{}).WithNamespace(context.Background(), " team-a ")
	if err != nil {
		t.Fatalf("WithNamespace() error = %v", err)
	}
	ns, ok := namespaces.Namespace(nsCtx)
	if !ok || ns != "team-a" {
		t.Fatalf("namespace = %q ok=%v, want team-a true", ns, ok)
	}
}
