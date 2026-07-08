package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/opencontainers/go-digest"
	snapshotSvc "github.com/openeuler/Conch/internal/adapters/containerd/plugins/snapshot"
	conchimage "github.com/openeuler/Conch/internal/image"
)

func TestHandleSnapshotExport(t *testing.T) {
	oldBoot := buildBootIndexArchive
	defer func() { buildBootIndexArchive = oldBoot }()
	buildBootIndexArchive = func(_ context.Context, opts conchimage.BootIndexOptions) (digest.Digest, error) {
		if len(opts.MemChainPaths) == 0 || len(opts.SandboxChainPaths) == 0 || opts.Tag == "" {
			t.Fatalf("boot options = %#v", opts)
		}
		return digest.FromString("snapshot"), os.WriteFile(opts.ArchivePath, []byte("snapshot-archive"), 0o644)
	}

	imgSvc := &fakeImageService{}
	snapSvc := &fakeSnapshotService{}
	manager := &fakeSandboxOps{}
	server := newConvertHandlerServer(imgSvc, snapSvc, manager)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/snapshot/export", bytes.NewBufferString(`{"sandbox_id":"sandbox-123","namespace":"team-a","boot_index_tag":"localhost/conch/snap:latest"}`))
	server.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if manager.pauseReq.SandboxId != "sandbox-123" || manager.pauseReq.Namespace != "team-a" {
		t.Fatalf("pause request = %#v", manager.pauseReq)
	}
	if snapSvc.infoReq.Key != "paused-rootfs-id" || snapSvc.chainReq.Namespace != "team-a" {
		t.Fatalf("snapshot requests info=%#v chain=%#v", snapSvc.infoReq, snapSvc.chainReq)
	}
	if imgSvc.exportReq.ImageName != "rootfs-image:latest" || imgSvc.exportReq.Namespace != "team-a" {
		t.Fatalf("export request = %#v", imgSvc.exportReq)
	}
	if len(imgSvc.importReqs) != 1 || imgSvc.importReqs[0].ImportedTag != "localhost/conch/snap:latest" {
		t.Fatalf("import requests = %#v", imgSvc.importReqs)
	}
	var got snapshotExportResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.BootIndexTag != "localhost/conch/snap:latest" || got.BootIndexDigest == "" {
		t.Fatalf("response = %#v", got)
	}
}

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
	req = httptest.NewRequest(http.MethodPost, "/api/snapshot/remove", bytes.NewBufferString(`{"namespace":"team-a","key":"sha256:rootfs","cascade":true}`))
	server.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if svc.removeReq.Namespace != "team-a" || svc.removeReq.Key != "sha256:rootfs" || !svc.removeReq.Cascade {
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
