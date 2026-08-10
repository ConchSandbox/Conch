package image

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestBuildBootIndexInContentWritesBootIndexBlobs(t *testing.T) {
	requireMkfsErofs(t)

	ctx := context.Background()
	dir := t.TempDir()
	store, err := localcontent.NewStore(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "bin"), []byte("rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	rootfsDesc, err := BuildNativeComponentInContent(ctx, store, []string{rootfs}, KindRootfs, "localhost/conch/rootfs:test")
	if err != nil {
		t.Fatalf("BuildNativeComponentInContent rootfs: %v", err)
	}

	kernel := filepath.Join(dir, "bzImage")
	initrd := filepath.Join(dir, "conch.initrd")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initrd, []byte("initrd"), 0o644); err != nil {
		t.Fatal(err)
	}

	indexDesc, err := BuildBootIndexInContent(ctx, store, BootIndexContentOptions{
		RootfsDescriptor: rootfsDesc,
		KernelPath:       kernel,
		InitrdPath:       initrd,
		Tag:              "localhost/conch/demo:latest",
	})
	if err != nil {
		t.Fatalf("BuildBootIndexInContent: %v", err)
	}
	raw, err := content.ReadBlob(ctx, store, indexDesc)
	if err != nil {
		t.Fatalf("read boot index blob: %v", err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatalf("unmarshal boot index: %v", err)
	}
	if len(index.Manifests) != 2 {
		t.Fatalf("manifest count = %d, want 2", len(index.Manifests))
	}
	kinds := map[string]bool{}
	for _, desc := range index.Manifests {
		kinds[desc.Annotations["io.conch.kind"]] = true
		if _, err := content.ReadBlob(ctx, store, desc); err != nil {
			t.Fatalf("manifest blob %s missing: %v", desc.Digest, err)
		}
	}
	if !kinds[KindRootfs] || !kinds[KindSandbox] {
		t.Fatalf("kinds = %#v", kinds)
	}
}

func TestBuildBootIndexInContentUsesPreparedCheckpointComponentsInStableOrder(t *testing.T) {
	requireMkfsErofs(t)

	ctx := context.Background()
	dir := t.TempDir()
	store, err := localcontent.NewStore(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	build := func(kind, name string) ocispec.Descriptor {
		t.Helper()
		desc, err := BuildNativeComponentInContent(ctx, store, []string{writeComponentRoot(t, dir, name)}, kind, "localhost/conch/checkpoint:"+name)
		if err != nil {
			t.Fatalf("BuildNativeComponentInContent(%s): %v", kind, err)
		}
		return desc
	}
	rootfsDesc := build(KindRootfs, "rootfs-capture")
	memDesc := build(KindMemSnapshot, "mem-capture")
	sandboxDesc := build(KindSandbox, "source-sandbox")

	indexDesc, err := BuildBootIndexInContent(ctx, store, BootIndexContentOptions{
		RootfsDescriptor:  rootfsDesc,
		MemDescriptor:     memDesc,
		SandboxDescriptor: sandboxDesc,
		Tag:               "localhost/conch/checkpoint:one",
		VMMName:           "cloud-hypervisor",
		MemorySizeMB:      512,
	})
	if err != nil {
		t.Fatalf("BuildBootIndexInContent: %v", err)
	}
	raw, err := content.ReadBlob(ctx, store, indexDesc)
	if err != nil {
		t.Fatalf("read boot index: %v", err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatalf("unmarshal boot index: %v", err)
	}
	wantKinds := []string{KindRootfs, KindMemSnapshot, KindSandbox}
	if len(index.Manifests) != len(wantKinds) {
		t.Fatalf("manifest count = %d, want %d", len(index.Manifests), len(wantKinds))
	}
	for i, want := range wantKinds {
		if got := getKind(index.Manifests[i]); got != want {
			t.Fatalf("manifest %d kind = %q, want %q", i, got, want)
		}
	}
	if got := index.Manifests[0].Digest; got != rootfsDesc.Digest {
		t.Fatalf("rootfs digest = %s, want reused %s", got, rootfsDesc.Digest)
	}
	if got := index.Manifests[2].Digest; got != sandboxDesc.Digest {
		t.Fatalf("sandbox digest = %s, want reused %s", got, sandboxDesc.Digest)
	}
	if got := index.Annotations[AnnotationVMM]; got != "cloud-hypervisor" {
		t.Fatalf("index VMM = %q", got)
	}
	if got := index.Manifests[1].Annotations[AnnotationVMM]; got != "cloud-hypervisor" {
		t.Fatalf("mem component VMM = %q", got)
	}
	if got := index.Annotations[AnnotationMemorySizeMB]; got != "512" {
		t.Fatalf("index memory size = %q", got)
	}
	if got := index.Manifests[1].Annotations[AnnotationMemorySizeMB]; got != "512" {
		t.Fatalf("mem component memory size = %q", got)
	}

	resolved, info, err := inspectBootIndexByDigest(ctx, store, indexDesc.Digest.String())
	if err != nil {
		t.Fatalf("inspectBootIndexByDigest: %v", err)
	}
	if resolved.Digest != indexDesc.Digest || resolved.Size != indexDesc.Size || resolved.MediaType != indexDesc.MediaType {
		t.Fatalf("resolved descriptor = %#v, want %#v", resolved, indexDesc)
	}
	if !info.Resume || info.VMMName != "cloud-hypervisor" || info.MemorySizeMB != 512 {
		t.Fatalf("boot index info = %#v", info)
	}
	if info.RootfsDescriptor.Digest != rootfsDesc.Digest {
		t.Fatalf("inspected rootfs digest = %s, want %s", info.RootfsDescriptor.Digest, rootfsDesc.Digest)
	}
	if info.SandboxDescriptor.Digest != sandboxDesc.Digest {
		t.Fatalf("inspected sandbox digest = %s, want %s", info.SandboxDescriptor.Digest, sandboxDesc.Digest)
	}
}

func TestBuildBootIndexInContentRejectsInvalidSandboxAndVMMCombinations(t *testing.T) {
	valid := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromString("component"),
		Size:      1,
	}
	cases := []struct {
		name string
		opts BootIndexContentOptions
		want string
	}{
		{
			name: "missing sandbox source",
			opts: BootIndexContentOptions{RootfsDescriptor: valid, Tag: "example:test"},
			want: "exactly one sandbox source",
		},
		{
			name: "prepared and kernel assets",
			opts: BootIndexContentOptions{RootfsDescriptor: valid, SandboxDescriptor: valid, KernelPath: "kernel", InitrdPath: "initrd", Tag: "example:test"},
			want: "exactly one sandbox source",
		},
		{
			name: "half kernel assets",
			opts: BootIndexContentOptions{RootfsDescriptor: valid, KernelPath: "kernel", Tag: "example:test"},
			want: "provided together",
		},
		{
			name: "mem without VMM",
			opts: BootIndexContentOptions{RootfsDescriptor: valid, MemDescriptor: valid, SandboxDescriptor: valid, Tag: "example:test"},
			want: "VMM name is required",
		},
		{
			name: "mem without memory size",
			opts: BootIndexContentOptions{RootfsDescriptor: valid, MemDescriptor: valid, SandboxDescriptor: valid, Tag: "example:test", VMMName: "stratovirt"},
			want: "positive memory size",
		},
		{
			name: "VMM without mem",
			opts: BootIndexContentOptions{RootfsDescriptor: valid, SandboxDescriptor: valid, Tag: "example:test", VMMName: "stratovirt"},
			want: "requires a mem-snapshot",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := localcontent.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			_, err = BuildBootIndexInContent(context.Background(), store, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildBootIndexInContent() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestBuildBootIndexInContentRejectsWrongPreparedComponentKind(t *testing.T) {
	requireMkfsErofs(t)

	ctx := context.Background()
	dir := t.TempDir()
	store, err := localcontent.NewStore(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatal(err)
	}
	rootfsDesc, err := BuildNativeComponentInContent(ctx, store, []string{writeComponentRoot(t, dir, "rootfs-kind")}, KindRootfs, "example:rootfs")
	if err != nil {
		t.Fatal(err)
	}
	sandboxDesc, err := BuildNativeComponentInContent(ctx, store, []string{writeComponentRoot(t, dir, "sandbox-kind")}, KindSandbox, "example:sandbox")
	if err != nil {
		t.Fatal(err)
	}
	sandboxDesc.Annotations["io.conch.kind"] = KindMemSnapshot

	_, err = BuildBootIndexInContent(ctx, store, BootIndexContentOptions{
		RootfsDescriptor:  rootfsDesc,
		SandboxDescriptor: sandboxDesc,
		Tag:               "example:boot",
	})
	if err == nil || !strings.Contains(err.Error(), `sandbox descriptor has component kind "mem-snapshot"`) {
		t.Fatalf("BuildBootIndexInContent() error = %v", err)
	}
}

func TestBuildNativeComponentInContentRejectsUnsafeInputs(t *testing.T) {
	store, err := localcontent.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildNativeComponentInContent(context.Background(), store, []string{"unused"}, "unknown", "example:unknown")
	if err == nil || !strings.Contains(err.Error(), "unsupported native component kind") {
		t.Fatalf("unknown kind error = %v", err)
	}

	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err = BuildNativeComponentInContent(context.Background(), store, []string{link}, KindMemSnapshot, "example:mem")
	if err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestInspectBootIndexContentRejectsMissingClosure(t *testing.T) {
	ctx := context.Background()
	store, err := localcontent.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	component := func(kind, value string) ocispec.Descriptor {
		return ocispec.Descriptor{
			MediaType:   ocispec.MediaTypeImageManifest,
			Digest:      digest.FromString(value),
			Size:        1,
			Annotations: map[string]string{"io.conch.kind": kind},
		}
	}
	indexDesc, err := writeIndexToContent(ctx, store, []ocispec.Descriptor{
		component(KindRootfs, "missing-rootfs"),
		component(KindSandbox, "missing-sandbox"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = InspectBootIndexContent(ctx, store, indexDesc)
	if err == nil || !strings.Contains(err.Error(), "closure") {
		t.Fatalf("InspectBootIndexContent() error = %v, want closure error", err)
	}
}

func TestInspectBootIndexContentRejectsMismatchedVMMCapability(t *testing.T) {
	requireMkfsErofs(t)

	ctx := context.Background()
	dir := t.TempDir()
	store, err := localcontent.NewStore(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatal(err)
	}
	build := func(kind, name string) ocispec.Descriptor {
		t.Helper()
		desc, err := BuildNativeComponentInContent(ctx, store, []string{writeComponentRoot(t, dir, name)}, kind, "example:"+name)
		if err != nil {
			t.Fatal(err)
		}
		return desc
	}
	rootfsDesc := build(KindRootfs, "rootfs-vmm")
	memDesc := build(KindMemSnapshot, "mem-vmm")
	memDesc.Annotations[AnnotationVMM] = "stratovirt"
	sandboxDesc := build(KindSandbox, "sandbox-vmm")
	indexDesc, err := writeIndexToContent(ctx, store, []ocispec.Descriptor{rootfsDesc, memDesc, sandboxDesc}, map[string]string{
		AnnotationVMM: "cloud-hypervisor",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = InspectBootIndexContent(ctx, store, indexDesc)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("InspectBootIndexContent() error = %v", err)
	}
}

func TestInspectBootIndexMemorySizeCompatibilityIsVMMSpecific(t *testing.T) {
	requireMkfsErofs(t)

	ctx := context.Background()
	dir := t.TempDir()
	store, err := localcontent.NewStore(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatal(err)
	}
	build := func(kind, name string) ocispec.Descriptor {
		t.Helper()
		desc, err := BuildNativeComponentInContent(ctx, store, []string{writeComponentRoot(t, dir, name)}, kind, "example:"+name)
		if err != nil {
			t.Fatal(err)
		}
		return desc
	}
	rootfsDesc := build(KindRootfs, "rootfs-memory-compat")
	memDesc := build(KindMemSnapshot, "mem-memory-compat")
	sandboxDesc := build(KindSandbox, "sandbox-memory-compat")

	for _, tt := range []struct {
		vmm       string
		wantError bool
	}{
		{vmm: "cloud-hypervisor"},
		{vmm: "stratovirt", wantError: true},
	} {
		t.Run(tt.vmm, func(t *testing.T) {
			candidateMem := memDesc
			annotations := map[string]string{AnnotationVMM: tt.vmm}
			if tt.vmm == "stratovirt" {
				annotations[AnnotationMemoryFormat] = MemoryFormatFullV1
			}
			candidateMem.Annotations = mergeAnnotations(memDesc.Annotations, annotations)
			indexDesc, err := writeIndexToContent(ctx, store, []ocispec.Descriptor{rootfsDesc, candidateMem, sandboxDesc}, map[string]string{
				AnnotationVMM: tt.vmm,
			})
			if err != nil {
				t.Fatal(err)
			}
			info, err := InspectBootIndexContent(ctx, store, indexDesc)
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), AnnotationMemorySizeMB) {
					t.Fatalf("InspectBootIndexContent() error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("InspectBootIndexContent() error = %v", err)
			}
			if info.MemorySizeMB != 0 {
				t.Fatalf("legacy CLH memory size = %d", info.MemorySizeMB)
			}
		})
	}
}

func TestInspectBootIndexStratoVirtMemoryFormat(t *testing.T) {
	requireMkfsErofs(t)
	ctx := context.Background()
	dir := t.TempDir()
	store, err := localcontent.NewStore(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatal(err)
	}
	build := func(kind, name string) ocispec.Descriptor {
		t.Helper()
		desc, err := BuildNativeComponentInContent(ctx, store, []string{writeComponentRoot(t, dir, name)}, kind, "example:"+name)
		if err != nil {
			t.Fatal(err)
		}
		return desc
	}
	rootfsDesc := build(KindRootfs, "rootfs-memory-format")
	memDesc := build(KindMemSnapshot, "mem-memory-format")
	sandboxDesc := build(KindSandbox, "sandbox-memory-format")
	tests := []struct {
		name       string
		vmm        string
		format     string
		wantFormat string
		wantError  bool
	}{
		{name: "full", vmm: "stratovirt", format: MemoryFormatFullV1, wantFormat: MemoryFormatFullV1},
		{name: "incremental", vmm: "stratovirt", format: MemoryFormatIncrementalV1, wantFormat: MemoryFormatIncrementalV1},
		{name: "missing", vmm: "stratovirt", wantError: true},
		{name: "unknown", vmm: "stratovirt", format: "layered-v1", wantError: true},
		{name: "cloud hypervisor", vmm: "cloud-hypervisor"},
		{name: "cloud hypervisor annotated", vmm: "cloud-hypervisor", format: MemoryFormatFullV1, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidateMem := memDesc
			candidateMem.Annotations = mergeAnnotations(memDesc.Annotations, map[string]string{
				AnnotationVMM:          test.vmm,
				AnnotationMemorySizeMB: "512",
			})
			if test.format != "" {
				candidateMem.Annotations[AnnotationMemoryFormat] = test.format
			}
			indexDesc, err := writeIndexToContent(ctx, store, []ocispec.Descriptor{rootfsDesc, candidateMem, sandboxDesc}, map[string]string{
				AnnotationVMM:          test.vmm,
				AnnotationMemorySizeMB: "512",
			})
			if err != nil {
				t.Fatal(err)
			}
			info, err := InspectBootIndexContent(ctx, store, indexDesc)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), AnnotationMemoryFormat) {
					t.Fatalf("InspectBootIndexContent() error = %v, want %s", err, AnnotationMemoryFormat)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if info.MemoryFormat != test.wantFormat {
				t.Fatalf("MemoryFormat = %q, want %q", info.MemoryFormat, test.wantFormat)
			}
		})
	}
}

func TestBuildIncrementalMemoryComponentAppendsOneLayer(t *testing.T) {
	requireMkfsErofs(t)
	ctx := context.Background()
	dir := t.TempDir()
	store, err := localcontent.NewStore(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := BuildIncrementalMemoryComponentInContent(ctx, store, ocispec.Descriptor{}, writeComponentRoot(t, dir, "epoch-a"), "example:a")
	if err != nil {
		t.Fatal(err)
	}
	first.Annotations = mergeAnnotations(first.Annotations, map[string]string{AnnotationMemoryFormat: MemoryFormatIncrementalV1})
	second, err := BuildIncrementalMemoryComponentInContent(ctx, store, first, writeComponentRoot(t, dir, "epoch-b"), "example:b")
	if err != nil {
		t.Fatal(err)
	}
	readManifest := func(desc ocispec.Descriptor) ocispec.Manifest {
		t.Helper()
		raw, err := content.ReadBlob(ctx, store, desc)
		if err != nil {
			t.Fatal(err)
		}
		var manifest ocispec.Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		return manifest
	}
	firstManifest := readManifest(first)
	secondManifest := readManifest(second)
	if len(firstManifest.Layers) != 1 || len(secondManifest.Layers) != 2 {
		t.Fatalf("layer counts = %d, %d", len(firstManifest.Layers), len(secondManifest.Layers))
	}
	if secondManifest.Layers[0].Digest != firstManifest.Layers[0].Digest {
		t.Fatalf("parent layer changed: first=%s second=%s", firstManifest.Layers[0].Digest, secondManifest.Layers[0].Digest)
	}
}

func writeComponentRoot(t *testing.T, dir, name string) string {
	t.Helper()
	root := filepath.Join(dir, name+"-root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte(name), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func requireMkfsErofs(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs not available")
	}
}
