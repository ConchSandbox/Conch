package e2bapi

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"

	"github.com/openeuler/Conch/internal/sandbox"
)

// TestNetworkHandlers covers update_network validation and state guards; a
// valid update replaces the egress policy and keeps the create-time host mask.
func TestNetworkHandlers(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	server := testServer(t, service, routes)
	native := seedRecord(t, service.Store, true, false, "")
	creating := seedRecord(t, service.Store, false, true, "")
	ready := seedRecord(t, service.Store, true, true, "")
	paused := seedPausedRecord(t, service.Store, digest.FromString("head-network").String())

	for _, tc := range []struct {
		name, id, body string
		status         int
	}{
		{"egress proxy", ready.ID, `{"egressProxy":{"url":"http://proxy"}}`, http.StatusBadRequest},
		{"rules", ready.ID, `{"rules":[{"port":80}]}`, http.StatusBadRequest},
		{"mask request host", ready.ID, `{"maskRequestHost":"example.com"}`, http.StatusBadRequest},
		{"unknown", uuid.NewString(), `{}`, http.StatusNotFound},
		{"native", native.ID, `{}`, http.StatusNotFound},
		{"creating", creating.ID, `{"allowOut":["1.1.1.1/32"]}`, http.StatusConflict},
		{"paused", paused.ID, `{}`, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := doRequest(t, server, http.MethodPut, "/sandboxes/"+tc.id+"/network", tc.body, true)
			if resp.StatusCode != tc.status {
				t.Fatalf("got HTTP %d %s, want %d", resp.StatusCode, data, tc.status)
			}
		})
	}
	runtime.mu.Lock()
	updates := append([]sandbox.NetworkUpdateRequest(nil), runtime.networkUpdates...)
	runtime.mu.Unlock()
	if len(updates) != 0 {
		t.Fatalf("rejected updates reached the runtime: %#v", updates)
	}

	ready.MaskRequestHost = "api.example.com:443"
	if _, err := service.Store.Update(t.Context(), ready); err != nil {
		t.Fatal(err)
	}
	resp, data := doRequest(t, server, http.MethodPut, "/sandboxes/"+ready.ID+"/network",
		`{"allowOut":["1.1.1.1/32"],"allow_internet_access":false}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("update = %d %s", resp.StatusCode, data)
	}
	updated, err := service.Store.Get(t.Context(), ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Network == nil || len(updated.Network.AllowOut) != 1 || updated.Network.AllowOut[0] != "1.1.1.1/32" ||
		len(updated.Network.DenyOut) != 0 {
		t.Fatalf("updated network = %#v", updated.Network)
	}
	if updated.Network.AllowInternetAccess == nil || *updated.Network.AllowInternetAccess {
		t.Fatalf("updated network = %#v, want allowInternetAccess=false", updated.Network)
	}
	if updated.MaskRequestHost != "api.example.com:443" {
		t.Fatalf("update cleared the create-time mask: %q", updated.MaskRequestHost)
	}
}
