package erofsconvert

import (
	"context"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestConvertRootfsRequiresClient(t *testing.T) {
	_, err := ConvertRootfs(context.Background(), nil, ConvertRootfsRequest{})
	if err == nil || !strings.Contains(err.Error(), "containerd client is required") {
		t.Fatalf("Convert() error = %v", err)
	}
}

func TestNormalizeRequestDefaults(t *testing.T) {
	req, err := NormalizeRequest(ConvertRootfsRequest{
		SourceImage: " source:latest ",
		TargetImage: " target:latest ",
	})
	if err != nil {
		t.Fatalf("NormalizeRequest: %v", err)
	}
	if req.SourceImage != "source:latest" || req.TargetImage != "target:latest" {
		t.Fatalf("request not trimmed: %#v", req)
	}
	if req.AlignBytes != DefaultAlignBytes {
		t.Fatalf("AlignBytes = %d, want %d", req.AlignBytes, DefaultAlignBytes)
	}
	if len(req.MkfsOptions) != 1 || req.MkfsOptions[0] != DefaultMkfsOption {
		t.Fatalf("MkfsOptions = %#v, want default", req.MkfsOptions)
	}
}

func TestNormalizeRequestRequiredFields(t *testing.T) {
	tests := []struct {
		name string
		req  ConvertRootfsRequest
		want string
	}{
		{
			name: "source",
			req:  ConvertRootfsRequest{TargetImage: "target"},
			want: "source image is required",
		},
		{
			name: "target",
			req:  ConvertRootfsRequest{SourceImage: "source"},
			want: "target image is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeRequest(tt.req)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NormalizeRequest error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNormalizeMkfsOptionsAddsDefaultOnce(t *testing.T) {
	got := NormalizeMkfsOptions([]string{"", "--foo", DefaultMkfsOption})
	want := []string{"--foo", DefaultMkfsOption}
	if len(got) != len(want) {
		t.Fatalf("options = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("options = %#v, want %#v", got, want)
		}
	}

	got = NormalizeMkfsOptions([]string{"--foo"})
	if len(got) != 2 || got[0] != DefaultMkfsOption || got[1] != "--foo" {
		t.Fatalf("options = %#v, want default first", got)
	}
}

func TestValidateNativeLayers(t *testing.T) {
	layers := []ocispec.Descriptor{
		{MediaType: NativeLayerMediaType, Digest: digest.FromString("layer0"), Size: DefaultAlignBytes},
		{MediaType: NativeLayerMediaType, Digest: digest.FromString("layer1"), Size: DefaultAlignBytes * 2},
	}
	got, err := ValidateNativeLayers(layers, DefaultAlignBytes)
	if err != nil {
		t.Fatalf("ValidateNativeLayers: %v", err)
	}
	if len(got) != 2 || got[0].Digest != layers[0].Digest.String() || got[1].Size != DefaultAlignBytes*2 {
		t.Fatalf("layers = %#v", got)
	}
}

func TestValidateNativeLayersRejectsInvalidManifest(t *testing.T) {
	tests := []struct {
		name   string
		layers []ocispec.Descriptor
		want   string
	}{
		{
			name:   "empty",
			layers: nil,
			want:   "no layers",
		},
		{
			name:   "tar layer",
			layers: []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayer, Digest: digest.FromString("tar"), Size: DefaultAlignBytes}},
			want:   "is not " + NativeLayerMediaType,
		},
		{
			name:   "unaligned",
			layers: []ocispec.Descriptor{{MediaType: NativeLayerMediaType, Digest: digest.FromString("small"), Size: 8192}},
			want:   "is not aligned",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateNativeLayers(tt.layers, DefaultAlignBytes)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateNativeLayers error = %v, want %q", err, tt.want)
			}
		})
	}
}
