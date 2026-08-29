package image

import (
	"context"
	"errors"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

func TestImageRepoDigests(t *testing.T) {
	tests := []struct {
		name   string
		ref    string
		digest string
		want   []string
	}{
		{
			name:   "tagged image",
			ref:    "registry.example.invalid/conch/demo:latest",
			digest: "sha256:demo",
			want:   []string{"registry.example.invalid/conch/demo@sha256:demo"},
		},
		{
			name:   "repo digest image",
			ref:    "registry.example.invalid/conch/demo@sha256:old",
			digest: "sha256:demo",
			want:   []string{"registry.example.invalid/conch/demo@sha256:demo"},
		},
		{
			name:   "digest only",
			ref:    "sha256:demo",
			digest: "sha256:demo",
		},
		{
			name:   "internal Template record",
			ref:    TemplateRecordName("registry.example:5000/team/busybox:latest"),
			digest: "sha256:demo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := imageRepoDigests(tt.ref, tt.digest)
			if len(got) != len(tt.want) {
				t.Fatalf("imageRepoDigests() = %#v, want %#v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("imageRepoDigests()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestTemplateRecordNameRoundTrip(t *testing.T) {
	const logicalName = "registry.example:5000/team/busybox:latest"
	recordName := TemplateRecordName(logicalName)
	if recordName != "io.conch.template/registry.example:5000/team/busybox:latest" {
		t.Fatalf("TemplateRecordName() = %q", recordName)
	}
	got, ok := TemplateNameFromRecordName(recordName)
	if !ok || got != logicalName {
		t.Fatalf("TemplateNameFromRecordName() = %q, %v", got, ok)
	}
	for _, invalid := range []string{"", TemplateRecordNamePrefix, logicalName} {
		if _, ok := TemplateNameFromRecordName(invalid); ok {
			t.Fatalf("TemplateNameFromRecordName(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestImageMutationsRejectInternalTemplateRecordName(t *testing.T) {
	client := &containerdclient.Client{Client: &containerd.Client{}}
	name := TemplateRecordName("registry.example:5000/team/busybox:latest")
	for _, test := range []struct {
		operation string
		run       func() error
	}{
		{
			operation: "pull",
			run: func() error {
				return Pull(context.Background(), client, runtimeapi.PullImageOptions{ImageName: name})
			},
		},
		{
			operation: "push",
			run: func() error {
				return Push(context.Background(), client, runtimeapi.PushImageOptions{LocalImage: name, RemoteImage: "registry.example/out:latest"})
			},
		},
		{
			operation: "remove",
			run: func() error {
				return Remove(context.Background(), client, runtimeapi.RemoveImageOptions{ImageName: name})
			},
		},
	} {
		t.Run(test.operation, func(t *testing.T) {
			if err := test.run(); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}
