package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRunImageListPrintsKind(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/image/list" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"images":[{"name":"localhost/conch/demo:latest","target_digest":"sha256:demo","size":42,"kind":"sandbox-snapshot"}]}`))
	}))
	defer server.Close()

	t.Setenv("CONCH_API_URL", server.URL)
	w2Buf := &bytes.Buffer{}
	oldStdout := os.Stdout
	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe2: %v", err)
	}
	os.Stdout = w2
	defer func() { os.Stdout = oldStdout }()

	if err := runImageList(context.Background(), nil); err != nil {
		t.Fatalf("runImageList() error = %v", err)
	}
	_ = w2.Close()
	if _, err := w2Buf.ReadFrom(r2); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}

	output := w2Buf.String()
	if !strings.HasPrefix(output, "NAME") {
		t.Fatalf("output should put NAME first:\n%s", output)
	}
	if !strings.Contains(output, "sandbox-snapshot") {
		t.Fatalf("output missing kind value:\n%s", output)
	}
}
