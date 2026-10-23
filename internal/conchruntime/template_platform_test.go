package conchruntime

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdtemplate "github.com/openeuler/Conch/internal/adapters/containerd/template"
	conchimage "github.com/openeuler/Conch/internal/image"
	"github.com/openeuler/Conch/internal/runtimeapi"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

// Real content, image and lease services exercise fetch/conversion without
// requiring a host kernel that supports the EROFS snapshotter.
func newPlatformTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	cs, err := local.NewStore(filepath.Join(dir, "content"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(dir, "metadata.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	md := metadata.NewDB(db, cs, nil)
	if err := md.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, err := containerd.New("", containerd.WithServices(containerd.WithContentStore(md.ContentStore()), containerd.WithImageStore(metadata.NewImageStore(md)), containerd.WithLeasesService(metadata.NewLeaseManager(md))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	wrapper := &containerdclient.Client{Client: c}
	s := New(nil, wrapper)
	s.Templates = containerdtemplate.NewStore(wrapper)
	return s
}

type platformRegistryBlob struct {
	data      []byte
	mediaType string
}

func platformRegistry(t *testing.T, cs content.Store, root ocispec.Descriptor) *httptest.Server {
	t.Helper()
	blobs := make(map[string]platformRegistryBlob)
	err := images.Walk(context.Background(), images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		data, err := content.ReadBlob(ctx, cs, desc)
		if err != nil {
			return nil, err
		}
		blobs[desc.Digest.String()] = platformRegistryBlob{data: data, mediaType: desc.MediaType}
		return images.ChildrenHandler(cs).Handle(ctx, desc)
	}), root)
	if err != nil {
		t.Fatal(err)
	}
	tags := map[string]string{"latest": root.Digest.String()}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			id := digest.FromBytes(data).String()
			blobs[id] = platformRegistryBlob{data: data, mediaType: r.Header.Get("Content-Type")}
			tags[ref] = id
			w.Header().Set("Docker-Content-Digest", id)
			w.WriteHeader(http.StatusCreated)
			return
		}
		if id, ok := tags[ref]; ok {
			ref = id
		}
		blob, ok := blobs[ref]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", blob.mediaType)
		w.Header().Set("Content-Length", fmt.Sprint(len(blob.data)))
		w.Header().Set("Docker-Content-Digest", ref)
		if r.Method != http.MethodHead {
			_, _ = w.Write(blob.data)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func writePlatformTestBlob(t *testing.T, cs content.Store, data []byte, mediaType string) ocispec.Descriptor {
	t.Helper()
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	if err := content.WriteBlob(containerdclient.NewNamespaceContext(context.Background()), cs, desc.Digest.String(), bytes.NewReader(data), desc); err != nil {
		t.Fatal(err)
	}
	return desc
}

func writePlatformTestJSON(t *testing.T, cs content.Store, value any, mediaType string) ocispec.Descriptor {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return writePlatformTestBlob(t, cs, raw, mediaType)
}

func platformTestBootIndex(t *testing.T, cs content.Store, value string) ocispec.Descriptor {
	t.Helper()
	path := filepath.Join(t.TempDir(), "layer.erofs")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := containerdclient.NewNamespaceContext(context.Background())
	root, err := conchimage.BuildNativeComponentInContent(ctx, cs, []string{path}, conchimage.KindRootfs, value)
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := conchimage.BuildNativeComponentInContent(ctx, cs, []string{path}, conchimage.KindSandbox, value)
	if err != nil {
		t.Fatal(err)
	}
	target, err := conchimage.BuildBootIndexInContent(ctx, cs, conchimage.BootIndexContentOptions{Platform: value, RootfsDescriptor: root, SandboxDescriptor: sandbox})
	if err != nil {
		t.Fatal(err)
	}
	p, err := platforms.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	target.Platform = &p
	return target
}

func TestTemplatePullKeepsSameNameAcrossPlatforms(t *testing.T) {
	ctx := context.Background()
	remote, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	x86, arm := platformTestBootIndex(t, remote, "linux/amd64"), platformTestBootIndex(t, remote, "linux/arm64")
	outer := writePlatformTestJSON(t, remote, ocispec.Index{Versioned: ispec.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{x86, arm}}, ocispec.MediaTypeImageIndex)
	registry := platformRegistry(t, remote, outer)
	name := strings.TrimPrefix(registry.URL, "http://") + "/template:latest"
	s := newPlatformTestService(t)
	for _, target := range []ocispec.Descriptor{arm, x86} {
		value := platforms.Format(*target.Platform)
		got, err := s.PullTemplate(ctx, TemplatePullOptions{Reference: name, Platform: value, PlainHTTP: true})
		if err != nil || got.Name != name || got.TemplateID != target.Digest.String() {
			t.Fatalf("Pull(%s) = %#v, %v", value, got, err)
		}
	}
	items, err := s.ListTemplates(ctx, runtimeapi.TemplateListOptions{})
	if err != nil || len(items) != 2 {
		t.Fatalf("List = %#v, %v", items, err)
	}
	if err := s.PushTemplate(ctx, TemplatePushOptions{Name: name, Platform: "linux/arm64", RemoteReference: strings.TrimSuffix(name, ":latest") + ":copy", PlainHTTP: true}); err != nil {
		t.Fatal(err)
	}
	response, err := http.Head(registry.URL + "/v2/template/manifests/copy")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.Header.Get("Docker-Content-Digest") != arm.Digest.String() {
		t.Fatal("push selected the wrong architecture")
	}
	foreign := "linux/arm64"
	foreignID := arm.Digest.String()
	if platforms.DefaultSpec().Architecture == "arm64" {
		foreign, foreignID = "linux/amd64", x86.Digest.String()
	}
	if err := s.UnpackTemplate(ctx, TemplateUnpackOptions{Name: name, Platform: foreign}); !errors.Is(err, conchtemplate.ErrInvalidArtifact) {
		t.Fatalf("foreign unpack must fail before accessing snapshot services: %v", err)
	}
	if _, err := conchimage.ResolveBoot(ctx, s.Containerd, foreignID); !errors.Is(err, conchimage.ErrInvalidArgument) {
		t.Fatalf("foreign boot must fail before accessing snapshot services: %v", err)
	}
	if err := s.RemoveTemplate(ctx, name, "linux/arm64"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTemplate(ctx, name, "linux/amd64"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateTemplateCrossPlatformFromRegistry(t *testing.T) {
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs is required")
	}
	ctx := containerdclient.NewNamespaceContext(context.Background())
	remote, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var manifests []ocispec.Descriptor
	for _, arch := range []string{"amd64", "arm64"} {
		var layer bytes.Buffer
		tw := tar.NewWriter(&layer)
		data := []byte("rootfs for " + arch)
		if err := tw.WriteHeader(&tar.Header{Name: "payload", Mode: 0o644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		ld := writePlatformTestBlob(t, remote, layer.Bytes(), ocispec.MediaTypeImageLayer)
		p := ocispec.Platform{OS: "linux", Architecture: arch}
		cfg := writePlatformTestJSON(t, remote, ocispec.Image{Platform: p, RootFS: ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{ld.Digest}}}, ocispec.MediaTypeImageConfig)
		manifest := writePlatformTestJSON(t, remote, ocispec.Manifest{Versioned: ispec.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, Config: cfg, Layers: []ocispec.Descriptor{ld}}, ocispec.MediaTypeImageManifest)
		manifest.Platform = &p
		manifests = append(manifests, manifest)
	}
	root := writePlatformTestJSON(t, remote, ocispec.Index{Versioned: ispec.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: manifests}, ocispec.MediaTypeImageIndex)
	registry := platformRegistry(t, remote, root)
	s := newPlatformTestService(t)
	foreign := "linux/arm64"
	if platforms.DefaultSpec().Architecture == "arm64" {
		foreign = "linux/amd64"
	}
	dir := t.TempDir()
	kernel, initrd := filepath.Join(dir, "kernel"), filepath.Join(dir, "initrd")
	for _, path := range []string{kernel, initrd} {
		if err := os.WriteFile(path, []byte("boot assets"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.CreateTemplate(ctx, TemplateCreateOptions{Name: "cross:latest", Platform: foreign, Source: strings.TrimPrefix(registry.URL, "http://") + "/rootfs:latest", PlainHTTP: true, KernelPath: kernel, InitrdPath: initrd})
	if err != nil {
		t.Fatal(err)
	}
	info, err := conchimage.InspectBootIndex(ctx, s.Containerd, got.TemplateID)
	if err != nil || info.Platform != foreign {
		t.Fatalf("cross build = %#v, %v", info, err)
	}
	entry, err := s.Templates.Get(ctx, "cross:latest", foreign)
	if err != nil || entry.Platform != foreign || entry.BootMode != conchtemplate.BootModeCold {
		t.Fatalf("cross template = %#v, %v", entry, err)
	}
	// No snapshot service is configured: succeeding proves cross-packaging
	// did not try to unpack or run foreign-architecture content.
}

func TestCreateTemplateRejectsInternalSourceBeforeFetching(t *testing.T) {
	s := newPlatformTestService(t)
	for _, platform := range []string{"", "linux/amd64", "linux/arm64"} {
		source := conchimage.TemplateRecordName("example.invalid/template:latest", platform)
		_, err := s.CreateTemplate(context.Background(), TemplateCreateOptions{
			Name: "output:latest", Source: source, KernelPath: "unused-kernel", InitrdPath: "unused-initrd",
		})
		if !errors.Is(err, conchtemplate.ErrInvalidArgument) {
			t.Fatalf("internal source %s = %v, want template.ErrInvalidArgument", source, err)
		}
	}
}
