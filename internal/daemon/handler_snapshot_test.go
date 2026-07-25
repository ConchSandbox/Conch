package daemon

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

func TestHandleListAndRemoveSnapshot(t *testing.T) {
	createdAt := time.Date(2026, time.July, 25, 1, 2, 3, 0, time.UTC)
	updatedAt := time.Date(2026, time.July, 25, 1, 2, 4, 0, time.UTC)
	svc := &fakeSnapshotService{
		snapshots: []runtimeapi.SnapshotRecord{{
			Key:         "sha256:rootfs",
			Kind:        "committed",
			Parent:      "sha256:parent",
			Labels:      map[string]string{"purpose": "rootfs"},
			StoragePath: "/snap/rootfs",
			CreatedAt:   createdAt,
			UpdatedAt:   updatedAt,
		}},
	}
	server := newSnapshotHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/list", bytes.NewBufferString(`{"namespace":"team-a","filters":["kind==committed"]}`))
	server.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.listReq.Namespace != "team-a" || len(svc.listReq.Filters) != 1 {
		t.Fatalf("list request = %#v", svc.listReq)
	}
	wantListJSON := "{\"snapshots\":[{\"key\":\"sha256:rootfs\",\"kind\":\"committed\",\"parent\":\"sha256:parent\",\"labels\":{\"purpose\":\"rootfs\"},\"storage_path\":\"/snap/rootfs\",\"created_at\":\"2026-07-25T01:02:03Z\",\"updated_at\":\"2026-07-25T01:02:04Z\"}]}\n"
	if got := rec.Body.String(); got != wantListJSON {
		t.Fatalf("list response = %q, want %q", got, wantListJSON)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/snapshot/remove", bytes.NewBufferString(`{"namespace":"team-a","key":"sha256:rootfs"}`))
	server.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.removeReq.Namespace != "team-a" || svc.removeReq.Key != "sha256:rootfs" {
		t.Fatalf("remove request = %#v", svc.removeReq)
	}
	if got, want := rec.Body.String(), "{\"status\":\"ok\"}\n"; got != want {
		t.Fatalf("remove response = %q, want %q", got, want)
	}
}

func TestHandleSnapshotInfo(t *testing.T) {
	createdAt := time.Date(2026, time.July, 25, 1, 2, 3, 0, time.UTC)
	updatedAt := time.Date(2026, time.July, 25, 1, 2, 4, 0, time.UTC)
	record := runtimeapi.SnapshotRecord{
		Key:         "sha256:rootfs",
		Kind:        "committed",
		Parent:      "sha256:parent",
		Labels:      map[string]string{"purpose": "rootfs"},
		StoragePath: "/snap/rootfs",
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	}
	svc := &fakeSnapshotService{
		infoResp: record,
	}
	server := newSnapshotHandlerServer(svc)
	recordJSON := "{\"key\":\"sha256:rootfs\",\"kind\":\"committed\",\"parent\":\"sha256:parent\",\"labels\":{\"purpose\":\"rootfs\"},\"storage_path\":\"/snap/rootfs\",\"created_at\":\"2026-07-25T01:02:03Z\",\"updated_at\":\"2026-07-25T01:02:04Z\"}"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/info", bytes.NewBufferString(`{"namespace":"team-a","key":"sha256:rootfs"}`))
	server.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("info status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.infoReq.Namespace != "team-a" || svc.infoReq.Key != "sha256:rootfs" {
		t.Fatalf("info request = %#v", svc.infoReq)
	}
	if got, want := rec.Body.String(), recordJSON+"\n"; got != want {
		t.Fatalf("info response = %q, want %q", got, want)
	}
}

func TestHandleRemoveSnapshotInvalidRequest(t *testing.T) {
	svc := &fakeSnapshotService{}
	server := newSnapshotHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/remove", bytes.NewBufferString(`{"namespace":"team-a"}`))
	server.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("remove status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if got, want := rec.Body.String(), "key is required\n"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
	if svc.removeCalls != 0 {
		t.Fatalf("remove calls = %d, want 0", svc.removeCalls)
	}
}

func TestHandleSnapshotInfoRequiresKey(t *testing.T) {
	server := newSnapshotHandlerServer(&fakeSnapshotService{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/info", bytes.NewBufferString(`{"namespace":"team-a"}`))
	server.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if got, want := rec.Body.String(), "key is required\n"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
}

func TestSnapshotChainRouteRemoved(t *testing.T) {
	server := newSnapshotHandlerServer(&fakeSnapshotService{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/chain", bytes.NewBufferString(`{"namespace":"team-a","key":"sha256:rootfs"}`))
	server.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}
