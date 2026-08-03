package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/image/client"
)

func TestRunImagePullPrintsProgressEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/image/pull/stream" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"status":"started"}` + "\n"))
		_, _ = w.Write([]byte(`{"status":"downloading","component":"rootfs","progress":40,"total":100}` + "\n"))
		_, _ = w.Write([]byte(`{"status":"downloading","component":"kernel","progress":10,"total":10}` + "\n"))
		_, _ = w.Write([]byte(`{"status":"unpacking"}` + "\n"))
		_, _ = w.Write([]byte(`{"status":"completed","results":{"rootfs":"rootfs-id"}}` + "\n"))
	}))
	defer server.Close()
	t.Setenv("CONCH_API_URL", server.URL)

	var buf bytes.Buffer
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	err = RunImagePull(context.Background(), []string{"hub.oepkgs.net/conch/demo:latest"})
	_ = w.Close()
	os.Stdout = oldStdout
	_, _ = buf.ReadFrom(r)
	if err != nil {
		t.Fatalf("RunImagePull: %v", err)
	}

	output := buf.String()
	for _, want := range []string{pullProgressFilled, pullProgressEmpty, "rootfs", "kernel", "40.0%", "100.0%", "40 B / 100 B", "10 B / 10 B", "Unpacking snapshots...", "rootfs-id"} {
		if !strings.Contains(output, want) {
			t.Fatalf("pull output missing %q:\n%s", want, output)
		}
	}
	if !strings.Contains(output, "["+strings.Repeat(pullProgressFilled, pullProgressBarWidth)+"]") {
		t.Fatalf("component progress output should include bracketed bar:\n%s", output)
	}
	if strings.Contains(output, "\033[") {
		t.Fatalf("pull output should not contain color escape:\n%s", output)
	}
	if strings.Contains(output, "still running") || strings.Contains(output, "pull started") {
		t.Fatalf("pull output contains verbose progress text:\n%s", output)
	}
}

func TestRunImagePullSkipUnpackUsesStreamWithoutUnpackMessage(t *testing.T) {
	var got client.PullImageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/image/pull/stream" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"status":"started"}` + "\n"))
		_, _ = w.Write([]byte(`{"status":"downloading","component":"rootfs","progress":100,"total":100}` + "\n"))
		_, _ = w.Write([]byte(`{"status":"completed","results":{}}` + "\n"))
	}))
	defer server.Close()
	t.Setenv("CONCH_API_URL", server.URL)

	var buf bytes.Buffer
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	err = RunImagePull(context.Background(), []string{"--skip-unpack", "hub.oepkgs.net/conch/demo:latest"})
	_ = w.Close()
	os.Stdout = oldStdout
	_, _ = buf.ReadFrom(r)
	if err != nil {
		t.Fatalf("RunImagePull: %v", err)
	}
	if !got.SkipUnpack {
		t.Fatalf("pull request did not set skip_unpack: %#v", got)
	}
	output := buf.String()
	if !strings.Contains(output, "Pulled image without unpacking") {
		t.Fatalf("skip-unpack success message missing:\n%s", output)
	}
	if strings.Contains(output, "Unpacking snapshots") {
		t.Fatalf("skip-unpack output should not enter unpacking:\n%s", output)
	}
}

func TestPullProgressBarsUseLineGlyphs(t *testing.T) {
	if pullProgressFilled != "━" || pullProgressEmpty != "─" {
		t.Fatalf("progress bars should use terminal-friendly line glyphs, got filled=%q empty=%q", pullProgressFilled, pullProgressEmpty)
	}
}

func TestRenderComponentProgress(t *testing.T) {
	got := renderComponentProgress("rootfs", 40, 100, false)
	for _, want := range []string{"rootfs", "40.0%", "40 B / 100 B", pullProgressFilled, pullProgressEmpty} {
		if !strings.Contains(got, want) {
			t.Fatalf("component progress missing %q: %q", want, got)
		}
	}
	if !strings.Contains(got, "[") || !strings.Contains(got, "]") {
		t.Fatalf("component progress should contain brackets: %q", got)
	}
}

func TestPullProgressRendererColorsFilledBarOnTTY(t *testing.T) {
	renderer := &pullProgressRenderer{
		tty: true,
		components: map[string]client.PullProgressEvent{
			"rootfs": {Component: "rootfs", Progress: 40, Total: 100},
		},
	}

	line := renderer.componentLines()[0]
	if !strings.Contains(line, "\033[32m"+pullProgressFilled) {
		t.Fatalf("completed progress should start in green: %q", line)
	}
	if !strings.Contains(line, pullProgressFilled+"\033[0m"+pullProgressEmpty) {
		t.Fatalf("color should reset before pending progress: %q", line)
	}
}

func TestPullProgressRendererKeepsComponentLinesCompact(t *testing.T) {
	renderer := &pullProgressRenderer{
		components: map[string]client.PullProgressEvent{
			"rootfs": {Component: "rootfs", Progress: 40, Total: 100},
			"kernel": {Component: "kernel", Progress: 10, Total: 10},
		},
	}

	lines := renderer.componentLines()
	if len(lines) != 2 {
		t.Fatalf("component lines should be compact without blank separators, got %#v", lines)
	}
	if !strings.Contains(lines[0], "rootfs") || !strings.Contains(lines[1], "kernel") {
		t.Fatalf("component lines missing expected components: %#v", lines)
	}
}

func TestPullProgressRendererIgnoresStartedEvent(t *testing.T) {
	var buf bytes.Buffer
	renderer := &pullProgressRenderer{
		out:        &buf,
		tty:        true,
		components: make(map[string]client.PullProgressEvent),
	}
	renderer.Handle(client.PullProgressEvent{Status: "started"})

	if got := buf.String(); got != "" {
		t.Fatalf("started event should not render progress output: %q", got)
	}
}

func TestPullProgressRendererHidesOverallWhenComponentsExist(t *testing.T) {
	var buf bytes.Buffer
	renderer := &pullProgressRenderer{
		out:        &buf,
		components: make(map[string]client.PullProgressEvent),
	}

	renderer.Handle(client.PullProgressEvent{Status: "downloading", Component: "rootfs", Progress: 40, Total: 100})
	renderer.Handle(client.PullProgressEvent{Status: "downloading", Component: "overall", Progress: 1, Total: 1})

	output := buf.String()
	if strings.Contains(output, "overall") {
		t.Fatalf("overall progress should be hidden when component progress exists:\n%s", output)
	}
	if !strings.Contains(output, "rootfs") {
		t.Fatalf("rootfs progress missing:\n%s", output)
	}
}

func TestPullProgressRendererKeepsComponentsBeforeUnpacking(t *testing.T) {
	var buf bytes.Buffer
	renderer := &pullProgressRenderer{
		out:        &buf,
		tty:        true,
		components: make(map[string]client.PullProgressEvent),
	}

	renderer.Handle(client.PullProgressEvent{Status: "downloading", Component: "rootfs", Progress: 40, Total: 100})
	renderer.Handle(client.PullProgressEvent{Status: "downloading", Component: "kernel", Progress: 10, Total: 10})
	renderer.Handle(client.PullProgressEvent{Status: "unpacking"})

	output := buf.String()
	if strings.Contains(output, "\033[2A") {
		t.Fatalf("unpacking should not clear existing component lines:\n%q", output)
	}
	if !strings.Contains(output, "rootfs") || !strings.Contains(output, "kernel") {
		t.Fatalf("download component lines should remain rendered before unpacking:\n%q", output)
	}
	if !strings.Contains(output, "Unpacking snapshots...") {
		t.Fatalf("unpacking message missing:\n%q", output)
	}
	if strings.Contains(output, "\n\nUnpacking snapshots...") {
		t.Fatalf("unpacking message should immediately follow component progress:\n%q", output)
	}
	if renderer.printedLines != 0 {
		t.Fatalf("unpacking should reset rendered component line count, got %d", renderer.printedLines)
	}
}

func TestPullProgressRendererIgnoresDownloadAfterUnpacking(t *testing.T) {
	var buf bytes.Buffer
	renderer := &pullProgressRenderer{
		out:        &buf,
		components: make(map[string]client.PullProgressEvent),
	}

	renderer.Handle(client.PullProgressEvent{Status: "unpacking"})
	renderer.Handle(client.PullProgressEvent{Status: "downloading", Component: "kernel", Progress: 10, Total: 10})

	output := buf.String()
	if strings.Contains(output, "kernel") || strings.Contains(output, "10 B / 10 B") {
		t.Fatalf("download progress after unpacking should be ignored:\n%s", output)
	}
	if !strings.Contains(output, "Unpacking snapshots...") {
		t.Fatalf("unpacking message missing:\n%s", output)
	}
}
