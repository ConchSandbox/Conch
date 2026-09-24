package e2bapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/openeuler/Conch/internal/adapters/bolt/volumestore"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/volume"
)

// newVolumeTestServer builds the standard test stack plus a real volume
// registry (bolt store in a temp dir) wired through SetVolumes.
func newVolumeTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	service, routes := testRuntime(t)
	server := testServer(t, service, routes)
	store, err := volumestore.Open(filepath.Join(t.TempDir(), "volumes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry, err := volume.NewRegistry(store, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	api := server.Config.Handler.(*Server)
	api.SetVolumes(registry)
	return api, server
}

func TestVolumeCRUDAndMountResolution(t *testing.T) {
	api, server := newVolumeTestServer(t)

	resp, data := doRequest(t, server, http.MethodPost, "/volumes", `{"name":"data"}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, data)
	}
	var created volumeCreateModel
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	if created.Name != "data" || created.VolumeID == "" || created.Token == "" {
		t.Fatalf("create model = %#v", created)
	}
	// Duplicate name conflicts.
	resp, _ = doRequest(t, server, http.MethodPost, "/volumes", `{"name":"data"}`, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate create = %d", resp.StatusCode)
	}
	// Name validations.
	for _, body := range []string{`{}`, `{"name":" "}`, `{"name":"` + uuid.NewString() + `"}`, `{"name":"data","extra":1}`, `null`} {
		resp, data = doRequest(t, server, http.MethodPost, "/volumes", body, true)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("create %s = %d %s", body, resp.StatusCode, data)
		}
	}

	// List returns the created volume.
	resp, data = doRequest(t, server, http.MethodGet, "/volumes", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d", resp.StatusCode)
	}
	var list []volumeModel
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].VolumeID != created.VolumeID || list[0].Name != "data" {
		t.Fatalf("list = %#v", list)
	}

	// Get by ID; unknown ID is 404.
	resp, data = doRequest(t, server, http.MethodGet, "/volumes/"+created.VolumeID, "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get = %d", resp.StatusCode)
	}
	var got volumeModel
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.VolumeID != created.VolumeID {
		t.Fatalf("get model = %#v", got)
	}
	resp, _ = doRequest(t, server, http.MethodGet, "/volumes/"+uuid.NewString(), "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get unknown = %d", resp.StatusCode)
	}

	// Delete removes the record; a second delete is 404.
	resp, _ = doRequest(t, server, http.MethodDelete, "/volumes/"+created.VolumeID, "", true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	resp, _ = doRequest(t, server, http.MethodGet, "/volumes/"+created.VolumeID, "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete = %d", resp.StatusCode)
	}
	resp, _ = doRequest(t, server, http.MethodDelete, "/volumes/"+created.VolumeID, "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("double delete = %d", resp.StatusCode)
	}

	// ResolveMounts turns names into host sources and rejects unknown names.
	resp, data = doRequest(t, server, http.MethodPost, "/volumes", `{"name":"shared"}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("second create = %d %s", resp.StatusCode, data)
	}
	resolved, err := api.volumes.ResolveMounts(t.Context(), []runtimeapi.VolumeMountSpec{{Name: "shared", Path: "/mnt/data"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].Name != "shared" || resolved[0].Source == "" || resolved[0].Path != "/mnt/data" {
		t.Fatalf("resolved = %#v", resolved)
	}
	if !filepath.IsAbs(resolved[0].Source) {
		t.Fatalf("source must be absolute: %q", resolved[0].Source)
	}
	if _, err := os.Stat(resolved[0].Source); err != nil {
		t.Fatalf("volume data directory missing: %v", err)
	}
	if _, err := api.volumes.ResolveMounts(t.Context(), []runtimeapi.VolumeMountSpec{{Name: "missing", Path: "/mnt/x"}}); err == nil {
		t.Fatal("unknown volume name resolved")
	}
}

// TestVolumeDeleteInUse proves a volume mounted by a live sandbox cannot be
// deleted (409).
func TestVolumeDeleteInUse(t *testing.T) {
	api, server := newVolumeTestServer(t)

	resp, data := doRequest(t, server, http.MethodPost, "/volumes", `{"name":"mounted"}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, data)
	}
	var created volumeCreateModel
	json.Unmarshal(data, &created)

	rec := seedRecord(t, api.runtime.Store, true, true, "volumes")
	rec.VolumeMounts = []sandbox.VolumeMountRecord{{Name: "mounted", Path: "/mnt/data"}}
	if _, err := api.runtime.Store.Update(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	resp, _ = doRequest(t, server, http.MethodDelete, "/volumes/"+created.VolumeID, "", true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete mounted = %d", resp.StatusCode)
	}
}

// TestCreateVolumeMountsValidation checks create-time volumeMounts payload
// handling: missing fields and unknown names are 400.
func TestCreateVolumeMountsValidation(t *testing.T) {
	_, server := newVolumeTestServer(t)

	resp, data := doRequest(t, server, http.MethodPost, "/volumes", `{"name":"data"}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, data)
	}
	for _, body := range []string{
		`{"templateID":"base","secure":false,"volumeMounts":[{"name":"data"}]}`,
		`{"templateID":"base","secure":false,"volumeMounts":[{"path":"/mnt"}]}`,
		`{"templateID":"base","secure":false,"volumeMounts":[{"name":"","path":""}]}`,
		`{"templateID":"base","secure":false,"volumeMounts":[{"name":"missing","path":"/mnt"}]}`,
	} {
		resp, data = doRequest(t, server, http.MethodPost, "/sandboxes", body, true)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("volumeMounts %s = %d %s", body, resp.StatusCode, data)
		}
	}
}

// TestVolumeEndpointsWithoutRegistry proves the /volumes API fails closed when
// the node has no volume registry configured.
func TestVolumeEndpointsWithoutRegistry(t *testing.T) {
	service, routes := testRuntime(t)
	server := testServer(t, service, routes)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/volumes"},
		{http.MethodGet, "/volumes"},
		{http.MethodGet, "/volumes/" + uuid.NewString()},
		{http.MethodDelete, "/volumes/" + uuid.NewString()},
	} {
		resp, data := doRequest(t, server, tc.method, tc.path, `{"name":"x"}`, true)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("%s %s = %d %s, want 501", tc.method, tc.path, resp.StatusCode, data)
		}
	}
}
