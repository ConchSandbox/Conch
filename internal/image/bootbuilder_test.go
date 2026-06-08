package image

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/openeuler/Conch/internal/util"
)

func TestBuildBootIndexArchiveAllowsNoMemSnapshot(t *testing.T) {
	requireMkfsErofs(t)

	dir := t.TempDir()
	rootfsArchive := writeSingleComponentArchive(t, dir, "rootfs", KindRootfs)
	sandboxRoot := writeComponentRoot(t, dir, "sandbox")
	bootArchive := filepath.Join(dir, "boot.oci.tar")
	if _, err := BuildBootIndexArchive(context.Background(), BootIndexOptions{
		RootfsArchivePath: rootfsArchive,
		SandboxChainPaths: []string{sandboxRoot},
		Tag:               "localhost/conch/demo:latest",
		ArchivePath:       bootArchive,
	}); err != nil {
		t.Fatalf("BuildBootIndexArchive: %v", err)
	}

	layoutDir := filepath.Join(dir, "boot-layout")
	if err := util.Untar(bootArchive, layoutDir); err != nil {
		t.Fatalf("Untar: %v", err)
	}
	index := readIndex(t, layoutDir, "index.json")
	if len(index.Manifests) != 2 {
		t.Fatalf("manifest count = %d, want 2", len(index.Manifests))
	}
	kinds := map[string]bool{}
	for _, desc := range index.Manifests {
		kinds[desc.Annotations["io.conch.kind"]] = true
	}
	if !kinds[KindRootfs] || !kinds[KindSandbox] || kinds[KindMemSnapshot] {
		t.Fatalf("kinds = %#v", kinds)
	}
}

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
	rootfsDesc, err := writeNativeComponentToContent(ctx, store, []string{rootfs}, KindRootfs, "localhost/conch/rootfs:test")
	if err != nil {
		t.Fatalf("writeNativeComponentToContent rootfs: %v", err)
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

func TestBuildBootIndexArchivePeelsNestedRootfsIndex(t *testing.T) {
	requireMkfsErofs(t)

	dir := t.TempDir()
	rootfsArchive := writeNestedIndexArchive(t, dir, "rootfs", KindRootfs)
	sandboxRoot := writeComponentRoot(t, dir, "sandbox")
	bootArchive := filepath.Join(dir, "boot.oci.tar")
	if _, err := BuildBootIndexArchive(context.Background(), BootIndexOptions{
		RootfsArchivePath: rootfsArchive,
		SandboxChainPaths: []string{sandboxRoot},
		Tag:               "localhost/conch/demo:latest",
		ArchivePath:       bootArchive,
	}); err != nil {
		t.Fatalf("BuildBootIndexArchive: %v", err)
	}

	layoutDir := filepath.Join(dir, "boot-layout")
	if err := util.Untar(bootArchive, layoutDir); err != nil {
		t.Fatalf("Untar: %v", err)
	}
	index := readIndex(t, layoutDir, "index.json")
	if got := index.Manifests[0].MediaType; got != ocispec.MediaTypeImageManifest {
		t.Fatalf("rootfs descriptor media type = %q, want %q", got, ocispec.MediaTypeImageManifest)
	}
}

func writeSingleComponentArchive(t *testing.T, dir, name, kind string) string {
	t.Helper()
	root := writeComponentRoot(t, dir, name)
	layoutDir := filepath.Join(dir, name+"-layout")
	desc, _, err := writeNativeComponent(context.Background(), layoutDir, []string{root}, kind, "localhost/conch/"+name+":latest")
	if err != nil {
		t.Fatal(err)
	}
	desc.Annotations = mergeAnnotations(desc.Annotations, map[string]string{"io.conch.kind": kind})
	if _, err := writeIndex(layoutDir, []ocispec.Descriptor{desc}); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(dir, name+".oci.tar")
	if err := util.TarDirectory(layoutDir, archivePath); err != nil {
		t.Fatal(err)
	}
	return archivePath
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

func writeNestedIndexArchive(t *testing.T, dir, name, kind string) string {
	t.Helper()
	archivePath := writeSingleComponentArchive(t, dir, name, kind)
	layoutDir := filepath.Join(dir, name+"-nested-layout")
	if err := util.Untar(archivePath, layoutDir); err != nil {
		t.Fatal(err)
	}
	index := readIndex(t, layoutDir, "index.json")
	nestedDesc, err := writeBlobJSON(layoutDir, index, ocispec.MediaTypeImageIndex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeIndex(layoutDir, []ocispec.Descriptor{nestedDesc}); err != nil {
		t.Fatal(err)
	}
	nestedArchive := filepath.Join(dir, name+"-nested.oci.tar")
	if err := util.TarDirectory(layoutDir, nestedArchive); err != nil {
		t.Fatal(err)
	}
	return nestedArchive
}

func readIndex(t *testing.T, layoutDir, name string) ocispec.Index {
	t.Helper()
	var index ocispec.Index
	raw, err := os.ReadFile(filepath.Join(layoutDir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatal(err)
	}
	return index
}

func readManifest(t *testing.T, layoutDir string, desc ocispec.Descriptor) ocispec.Manifest {
	t.Helper()
	var manifest ocispec.Manifest
	raw, err := os.ReadFile(filepath.Join(layoutDir, "blobs", "sha256", desc.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func requireMkfsErofs(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs not available")
	}
}
