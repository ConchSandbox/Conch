package daemon

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	containerdclient "github.com/openeuler/Conch/internal/adapters/containerd/client"
	containerdhost "github.com/openeuler/Conch/internal/adapters/containerd/host"
	"github.com/openeuler/Conch/internal/cli"
	"github.com/openeuler/Conch/internal/cli/client"
	"github.com/openeuler/Conch/internal/conchruntime"
	conchimage "github.com/openeuler/Conch/internal/image"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

func TestTemplateSaveLoadEndToEnd(t *testing.T) {
	ctx := context.Background()
	sourceHost := startTemplateHost(t)
	destHost := startTemplateHost(t)
	bootIndexDigest := buildE2EColdBootIndex(t, sourceHost, "offline-template")
	const (
		name      = "localhost/conch/offline:latest"
		sourceRef = "docker.io/library/busybox:latest"
	)
	parent := digest.FromString("offline-parent").String()
	if _, err := sourceHost.TemplateStore().Put(ctx, conchtemplate.Entry{
		Name:                  name,
		Origin:                conchtemplate.OriginImage,
		BootMode:              conchtemplate.BootModeCold,
		BootIndexDigest:       bootIndexDigest,
		ParentBootIndexDigest: parent,
		SourceRef:             sourceRef,
		Labels:                map[string]string{"role": "base"},
	}, bootIndexDescriptor(t, sourceHost, bootIndexDigest)); err != nil {
		t.Fatalf("put source template: %v", err)
	}

	sourceAPI := newTemplateAPIServer(t, sourceHost)
	destAPI := newTemplateAPIServer(t, destHost)
	configPath := writeE2EConfig(t)
	archivePath := filepath.Join(t.TempDir(), "templates.tar")

	saveOut, saveErr, saveCode := runConchCLI(t,
		"template", "save", "-o", archivePath,
		"-api-url", sourceAPI.URL, "-config", configPath, name,
	)
	if saveCode != 0 || strings.TrimSpace(saveOut) != "" {
		t.Fatalf("save code %d stdout %q stderr %q", saveCode, saveOut, saveErr)
	}
	info, err := os.Stat(archivePath)
	if err != nil || info.Size() == 0 {
		t.Fatalf("archive stat %v size %d", err, info.Size())
	}

	loadOut, loadErr, loadCode := runConchCLI(t,
		"template", "load", "-i", archivePath,
		"-api-url", destAPI.URL, "-config", configPath,
	)
	if loadCode != 0 {
		t.Fatalf("load code %d stdout %q stderr %q", loadCode, loadOut, loadErr)
	}
	if strings.TrimSpace(loadOut) != "Loaded image: "+name {
		t.Fatalf("load stdout = %q", loadOut)
	}
	assertLoadedTemplate(t, ctx, destAPI.URL, configPath, name, bootIndexDigest, parent, sourceRef)

	gzipPath := archivePath + ".gz"
	gzipFile(t, archivePath, gzipPath)
	gzipOut, gzipErr, gzipCode := runConchCLI(t,
		"template", "load", "-i", gzipPath,
		"-api-url", destAPI.URL, "-config", configPath,
	)
	if gzipCode != 0 || strings.TrimSpace(gzipOut) != "Loaded image: "+name {
		t.Fatalf("gzip load code %d stdout %q stderr %q", gzipCode, gzipOut, gzipErr)
	}
	assertLoadedTemplate(t, ctx, destAPI.URL, configPath, name, bootIndexDigest, parent, sourceRef)

	missingPath := filepath.Join(t.TempDir(), "missing.tar")
	_, missingErr, missingCode := runConchCLI(t,
		"template", "save", "-o", missingPath,
		"-api-url", sourceAPI.URL, "-config", configPath, "localhost/conch/missing:latest",
	)
	if missingCode == 0 || !strings.Contains(missingErr, "not found") {
		t.Fatalf("missing save code %d stderr %q", missingCode, missingErr)
	}
	if _, err := os.Stat(missingPath); !os.IsNotExist(err) {
		t.Fatalf("failed save left archive: %v", err)
	}
}

func TestTemplateSaveEmptyNamesStaysJSON(t *testing.T) {
	server := &Daemon{router: http.NewServeMux(), runtimeService: conchruntime.New(nil, nil)}
	server.routes()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/template/save", strings.NewReader(`{"names":[]}`))
	server.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body %s", recorder.Code, recorder.Body.String())
	}
	var response apiErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Code != "template.invalid_argument" {
		t.Fatalf("body %s err %v", recorder.Body.String(), err)
	}
}

func TestWriteTemplateSaveResponseStreamsOnce(t *testing.T) {
	recorder := httptest.NewRecorder()
	if err := writeTemplateSaveResponse(recorder, func(w io.Writer) error {
		_, err := io.WriteString(w, "tar-bytes")
		return err
	}); err != nil {
		t.Fatalf("writeTemplateSaveResponse: %v", err)
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != "tar-bytes" ||
		recorder.Header().Get("Content-Type") != "application/x-tar" ||
		recorder.Header().Get("Content-Length") != "" {
		t.Fatalf("status %d headers %#v body %q", recorder.Code, recorder.Header(), recorder.Body.String())
	}

	failed := httptest.NewRecorder()
	if err := writeTemplateSaveResponse(failed, func(io.Writer) error {
		return fmt.Errorf("missing template")
	}); err == nil || failed.Body.Len() != 0 {
		t.Fatalf("pre-stream error = %v body %q", err, failed.Body.String())
	}

	defer func() {
		recovered := recover()
		if recovered != http.ErrAbortHandler {
			t.Fatalf("mid-stream failure recovered %#v, want ErrAbortHandler", recovered)
		}
	}()
	_ = writeTemplateSaveResponse(httptest.NewRecorder(), func(w io.Writer) error {
		_, _ = io.WriteString(w, "partial")
		return fmt.Errorf("disk full")
	})
	t.Fatal("mid-stream failure returned instead of aborting")
}

func assertLoadedTemplate(t *testing.T, ctx context.Context, apiURL, configPath, name, bootIndexDigest, parent, sourceRef string) {
	t.Helper()
	conchClient, err := client.New(client.Options{BaseURL: apiURL, ConfigPath: configPath})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	item, err := conchClient.InspectTemplate(ctx, name)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if item.Name != name || item.TemplateID != bootIndexDigest || item.Origin != "image" || item.BootMode != "cold" ||
		item.ParentTemplateID != parent || item.SourceRef != sourceRef || item.Labels["role"] != "base" {
		t.Fatalf("inspect = %#v", item)
	}
}

func newTemplateAPIServer(t *testing.T, host *containerdhost.Host) *httptest.Server {
	t.Helper()
	svc := conchruntime.New(nil, host.Client())
	svc.Templates = host.TemplateStore()
	server := &Daemon{
		router:         http.NewServeMux(),
		runtimeService: svc,
		daemonClient:   host.Client(),
	}
	server.routes()
	httpServer := httptest.NewServer(server.router)
	t.Cleanup(httpServer.Close)
	return httpServer
}

func startTemplateHost(t *testing.T) *containerdhost.Host {
	t.Helper()
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skip("mkfs.erofs is required")
	}
	host, err := containerdhost.Start(context.Background(), containerdhost.Config{
		RootDir:  t.TempDir(),
		StateDir: t.TempDir(),
		Snapshot: containerdhost.SnapshotConfig{WorkDir: t.TempDir()},
	})
	if err != nil {
		t.Skipf("embedded containerd host unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := host.Close(); err != nil {
			t.Errorf("close containerd host: %v", err)
		}
	})
	return host
}

func buildE2EColdBootIndex(t *testing.T, host *containerdhost.Host, payload string) string {
	t.Helper()
	ctx := containerdclient.NewNamespaceContext(context.Background())
	leaseCtx, done, err := host.Client().WithLease(ctx)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	t.Cleanup(func() { _ = done(leaseCtx) })
	store := host.Client().ContentStore()
	rootfsDir := filepath.Join(t.TempDir(), "rootfs")
	sandboxDir := filepath.Join(t.TempDir(), "sandbox")
	for _, dir := range []string{rootfsDir, sandboxDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "payload"), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rootfsDesc, err := conchimage.BuildNativeComponentInContent(leaseCtx, store, []string{rootfsDir}, conchimage.KindRootfs)
	if err != nil {
		t.Fatalf("rootfs: %v", err)
	}
	sandboxDesc, err := conchimage.BuildNativeComponentInContent(leaseCtx, store, []string{sandboxDir}, conchimage.KindSandbox)
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	indexDesc, err := conchimage.BuildBootIndexInContent(leaseCtx, store, conchimage.BootIndexContentOptions{
		RootfsDescriptor:  rootfsDesc,
		SandboxDescriptor: sandboxDesc,
	})
	if err != nil {
		t.Fatalf("boot index: %v", err)
	}
	return indexDesc.Digest.String()
}

func bootIndexDescriptor(t *testing.T, host *containerdhost.Host, bootIndexDigest string) ocispec.Descriptor {
	t.Helper()
	info, err := host.Client().ContentStore().Info(containerdclient.NewNamespaceContext(context.Background()), digest.Digest(bootIndexDigest))
	if err != nil {
		t.Fatalf("boot index info: %v", err)
	}
	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.Digest(bootIndexDigest), Size: info.Size}
}

func writeE2EConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("app:\n  name: conch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func gzipFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	if _, err := io.Copy(gz, in); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func runConchCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdoutW, stderrW
	code := cli.Run(args)
	_ = stdoutW.Close()
	_ = stderrW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	var outBuf, errBuf bytes.Buffer
	if _, err := io.Copy(&outBuf, stdoutR); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(&errBuf, stderrR); err != nil {
		t.Fatal(err)
	}
	return outBuf.String(), errBuf.String(), code
}
