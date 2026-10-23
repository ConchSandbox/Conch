package platformutil

import (
	"testing"

	"github.com/containerd/platforms"
)

func TestNormalize(t *testing.T) {
	for input, want := range map[string]string{
		"": platforms.DefaultString(), "amd64": "linux/amd64", "linux/x86_64": "linux/amd64",
		"aarch64": "linux/arm64", "linux/arm64/v8": "linux/arm64", " linux/arm64 ": "linux/arm64",
	} {
		t.Run(input, func(t *testing.T) {
			got, err := Normalize(input)
			if err != nil || got != want {
				t.Fatalf("Normalize(%q) = %q, %v; want %q", input, got, err, want)
			}
		})
	}
	for _, input := range []string{"windows/amd64", "linux/arm/v7", "linux/ppc64le", "linux/arm64/v9", "linux/amd64/v3", "linux/arm64/invalid", "linux//arm64"} {
		if _, err := Normalize(input); err == nil {
			t.Errorf("Normalize(%q) accepted unsupported platform", input)
		}
	}
}
