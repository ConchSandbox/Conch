package e2bapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/openeuler/Conch/internal/conchruntime"
	conchtemplate "github.com/openeuler/Conch/internal/template"
)

// snapshotTemplateStore fakes the template store for the snapshot list: List
// honors the origin filter like the real store; sizing enrichment is skipped
// because the service has no containerd client in this harness.
type snapshotTemplateStore struct {
	conchtemplate.Store
	entries []conchtemplate.Entry
}

func (s snapshotTemplateStore) List(_ context.Context, filter conchtemplate.Filter) ([]conchtemplate.Entry, error) {
	out := make([]conchtemplate.Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		if filter.Origin != "" && entry.Origin != filter.Origin {
			continue
		}
		if filter.BootMode != "" && entry.BootMode != filter.BootMode {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

func TestListSnapshots(t *testing.T) {
	service, routes := testRuntime(t)
	base := time.Now().Add(-time.Hour).UnixNano()
	service.Templates = snapshotTemplateStore{entries: []conchtemplate.Entry{
		{Name: "checkpoint-named-a", Origin: conchtemplate.OriginCheckpoint, BootMode: conchtemplate.BootModeResume,
			BootIndexDigest: digest.FromString("named-a").String(), SourceSandboxID: "sandbox-a",
			Labels: map[string]string{conchruntime.SnapshotAliasLabel: "release-1"}, CreatedAt: base},
		{Name: "checkpoint-auto-a", Origin: conchtemplate.OriginCheckpoint, BootMode: conchtemplate.BootModeResume,
			BootIndexDigest: digest.FromString("auto-a").String(), SourceSandboxID: "sandbox-a",
			CreatedAt: base + int64(time.Minute)},
		{Name: "release-2", Origin: conchtemplate.OriginCheckpoint, BootMode: conchtemplate.BootModeResume,
			BootIndexDigest: digest.FromString("named-b").String(), SourceSandboxID: "sandbox-b",
			Labels: map[string]string{conchruntime.SnapshotAliasLabel: "release-2"}, CreatedAt: base + int64(2*time.Minute)},
		{Name: "cold-image", Origin: conchtemplate.OriginImage, BootMode: conchtemplate.BootModeCold,
			BootIndexDigest: digest.FromString("image").String(), SourceSandboxID: "sandbox-a",
			CreatedAt: base + int64(3*time.Minute)},
	}}
	server := testServer(t, service, routes)

	resp, _ := doRequest(t, server, http.MethodGet, "/snapshots", "", false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated snapshots = %d", resp.StatusCode)
	}

	resp, data := doRequest(t, server, http.MethodGet, "/snapshots", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s", resp.StatusCode, data)
	}
	var page []snapshotModel
	if err := json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	if len(page) != 3 {
		t.Fatalf("page = %d items, want 3 (image templates excluded)", len(page))
	}
	// Newest first; sizing is zero without containerd enrichment.
	for i, want := range []struct {
		snapshotID string
		names      []string
	}{
		{"release-2", []string{"release-2"}},
		{"checkpoint-auto-a", []string{}},
		{"checkpoint-named-a", []string{"release-1"}},
	} {
		if page[i].SnapshotID != want.snapshotID || len(page[i].Names) != len(want.names) || (len(want.names) > 0 && page[i].Names[0] != want.names[0]) {
			t.Fatalf("page[%d] = %#v, want %s %v", i, page[i], want.snapshotID, want.names)
		}
		if page[i].CPUCount != 0 || page[i].MemoryMB != 0 {
			t.Fatalf("page[%d] sizing = %d/%d, want zeros", i, page[i].CPUCount, page[i].MemoryMB)
		}
		if _, err := time.Parse(time.RFC3339Nano, page[i].CreatedAt); err != nil {
			t.Fatalf("page[%d] createdAt = %q", i, page[i].CreatedAt)
		}
	}

	// sandboxID and name filters (name matches the template name or alias).
	for _, tc := range []struct {
		query       string
		wantOffsets []int
	}{
		{"sandboxID=sandbox-a", []int{1, 2}},
		{"name=release-1", []int{2}},
		{"name=checkpoint-auto-a", []int{1}},
		{"sandboxID=sandbox-c", nil},
	} {
		resp, data = doRequest(t, server, http.MethodGet, "/snapshots?"+tc.query, "", true)
		if err := json.Unmarshal(data, &page); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %s (%v)", tc.query, resp.StatusCode, data, err)
		}
		if len(page) != len(tc.wantOffsets) {
			t.Fatalf("%s = %d items, want %d", tc.query, len(page), len(tc.wantOffsets))
		}
		for i, offset := range tc.wantOffsets {
			wantID := []string{"release-2", "checkpoint-auto-a", "checkpoint-named-a"}[offset]
			if page[i].SnapshotID != wantID {
				t.Fatalf("%s page[%d] = %s, want %s", tc.query, i, page[i].SnapshotID, wantID)
			}
		}
	}

	// Pagination: limit 2 returns a token whose page completes the list.
	seen := map[string]bool{}
	token := ""
	pages := 0
	for {
		path := "/snapshots?limit=2"
		if token != "" {
			path += "&nextToken=" + token
		}
		resp, data = doRequest(t, server, http.MethodGet, path, "", true)
		if err := json.Unmarshal(data, &page); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d %s (%v)", path, resp.StatusCode, data, err)
		}
		for _, item := range page {
			if seen[item.SnapshotID] {
				t.Fatalf("duplicate %s across pages", item.SnapshotID)
			}
			seen[item.SnapshotID] = true
		}
		pages++
		token = resp.Header.Get("X-Next-Token")
		if token == "" {
			break
		}
	}
	if len(seen) != 3 || pages != 2 {
		t.Fatalf("pagination saw %d items in %d pages", len(seen), pages)
	}

	for _, query := range []string{"limit=0", "limit=101", "limit=abc", "nextToken=invalid"} {
		resp, data = doRequest(t, server, http.MethodGet, "/snapshots?"+query, "", true)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("invalid query %q = HTTP %d %s", query, resp.StatusCode, data)
		}
	}
	resp, data = doRequest(t, server, http.MethodDelete, "/snapshots", "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete snapshots = %d, want 404", resp.StatusCode)
	}
}

// TestSnapshotsRequiresTemplateStore proves the endpoint surfaces a missing
// template store as a server error instead of an empty list.
func TestSnapshotsRequiresTemplateStore(t *testing.T) {
	service, routes := testRuntime(t)
	server := testServer(t, service, routes)
	resp, data := doRequest(t, server, http.MethodGet, "/snapshots", "", true)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("unconfigured store = %d %s", resp.StatusCode, data)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
}
