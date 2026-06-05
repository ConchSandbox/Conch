package image

import (
	"net/http"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestClassifyConchIndexKind(t *testing.T) {
	base := classifyConchIndexKind(ocispec.Index{
		Manifests: []ocispec.Descriptor{
			{Annotations: map[string]string{"io.conch.kind": "rootfs"}},
			{Annotations: map[string]string{"io.conch.kind": "sandbox"}},
		},
	})
	if base != "sandbox-base" {
		t.Fatalf("base kind = %q, want sandbox-base", base)
	}

	snapshot := classifyConchIndexKind(ocispec.Index{
		Manifests: []ocispec.Descriptor{
			{Annotations: map[string]string{"io.conch.kind": "rootfs"}},
			{Annotations: map[string]string{"io.conch.kind": "sandbox"}},
			{Annotations: map[string]string{"io.conch.kind": "mem-snapshot"}},
		},
	})
	if snapshot != "sandbox-snapshot" {
		t.Fatalf("snapshot kind = %q, want sandbox-snapshot", snapshot)
	}

	invalid := classifyConchIndexKind(ocispec.Index{
		Manifests: []ocispec.Descriptor{
			{Annotations: map[string]string{"io.conch.kind": "rootfs"}},
		},
	})
	if invalid != "" {
		t.Fatalf("invalid kind = %q, want empty", invalid)
	}
}

func TestInferComponentKindFromName(t *testing.T) {
	cases := map[string]string{
		"localhost/conch/rootfs-component:abc":       "rootfs",
		"localhost/conch/demo:latest-rootfs":         "rootfs",
		"localhost/conch/sandbox-component:def":      "sandbox",
		"localhost/conch/demo:latest-sandbox":        "sandbox",
		"localhost/conch/mem-snapshot-component:ghi": "mem-snapshot",
		"localhost/conch/demo:latest-mem":            "mem-snapshot",
		"localhost/conch/demo:latest":                "",
	}
	for name, want := range cases {
		if got := inferComponentKindFromName(name); got != want {
			t.Fatalf("%s => %q, want %q", name, got, want)
		}
	}
}

func TestResolveRegistryResponseHeaderTimeout(t *testing.T) {
	t.Setenv(registryTimeoutEnv, "10m")
	if got := resolveRegistryResponseHeaderTimeout(""); got != 10*time.Minute {
		t.Fatalf("timeout = %s, want 10m", got)
	}

	t.Setenv(registryTimeoutEnv, "bad")
	if got := resolveRegistryResponseHeaderTimeout(""); got != defaultRegistryResponseHeaderTimeout {
		t.Fatalf("timeout = %s, want default %s", got, defaultRegistryResponseHeaderTimeout)
	}

	t.Setenv(registryTimeoutEnv, "0")
	if got := resolveRegistryResponseHeaderTimeout(""); got != defaultRegistryResponseHeaderTimeout {
		t.Fatalf("timeout = %s, want default %s", got, defaultRegistryResponseHeaderTimeout)
	}

	t.Setenv(registryTimeoutEnv, "10m")
	if got := resolveRegistryResponseHeaderTimeout("3m"); got != 3*time.Minute {
		t.Fatalf("request registry timeout = %s, want 3m", got)
	}
}

func TestRegistryHTTPClientUsesResolvedResponseHeaderTimeout(t *testing.T) {
	t.Setenv(registryTimeoutEnv, "7m")

	client := registryHTTPClient("")
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.ResponseHeaderTimeout != 7*time.Minute {
		t.Fatalf("ResponseHeaderTimeout = %s, want 7m", transport.ResponseHeaderTimeout)
	}
}
