package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	snapshotSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/snapshot"
)

func TestHandleListAndRemoveSnapshot(t *testing.T) {
	svc := &fakeSnapshotService{
		snapshots: []snapshotSvc.Meta{{
			Key:    "sha256:rootfs",
			Kind:   "committed",
			Parent: "sha256:parent",
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
	var listResp struct {
		Snapshots []snapshotSvc.Meta `json:"snapshots"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listResp.Snapshots) != 1 || listResp.Snapshots[0].Key != "sha256:rootfs" {
		t.Fatalf("list response = %#v", listResp)
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
}

func TestHandleRemoveSnapshotInvalidRequest(t *testing.T) {
	svc := &fakeSnapshotService{
		removeErr: errors.Join(snapshotSvc.ErrInvalidRequest, errors.New("key is required")),
	}
	server := newSnapshotHandlerServer(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/remove", bytes.NewBufferString(`{"namespace":"team-a"}`))
	server.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("remove status = %d, want %d, body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if svc.removeReq.Namespace != "team-a" || svc.removeReq.Key != "" {
		t.Fatalf("remove request = %#v", svc.removeReq)
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
