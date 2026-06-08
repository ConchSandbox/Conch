package image

import (
	"testing"

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
