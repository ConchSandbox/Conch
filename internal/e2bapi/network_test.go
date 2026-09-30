package e2bapi

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestNetworkHandlers covers update_network validation and state guards. The
// final case reaches the runtime layer, which is intentionally unconfigured in
// this unit fixture; a failure response proves the request passed API validation.
func TestNetworkHandlers(t *testing.T) {
	service, routes := testRuntime(t)
	server := testServer(t, service, routes)
	native := seedRecord(t, service.Store, true, false, "")
	creating := seedRecord(t, service.Store, false, true, "")
	ready := seedRecord(t, service.Store, true, true, "")

	for _, tc := range []struct {
		name, id, body string
		status         int
	}{
		{"egress proxy", ready.ID, `{"egressProxy":{"url":"http://proxy"}}`, http.StatusBadRequest},
		{"rules", ready.ID, `{"rules":[{"port":80}]}`, http.StatusBadRequest},
		{"creating", creating.ID, `{"allowOut":["1.1.1.1/32"]}`, http.StatusConflict},
		{"unknown", uuid.NewString(), `{}`, http.StatusNotFound},
		{"native", native.ID, `{}`, http.StatusNotFound},
		{"unknown field", ready.ID, `{"allowPublicTraffic":false}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := doRequest(t, server, http.MethodPut, "/sandboxes/"+tc.id+"/network", tc.body, true)
			if resp.StatusCode != tc.status {
				t.Fatalf("got HTTP %d %s, want %d", resp.StatusCode, data, tc.status)
			}
		})
	}

	resp, data := doRequest(t, server, http.MethodPut, "/sandboxes/"+ready.ID+"/network", `{"allowOut":["1.1.1.1/32"],"denyOut":["0.0.0.0/0"],"allow_internet_access":false}`, true)
	if resp.StatusCode < http.StatusBadRequest {
		t.Fatalf("unconfigured runtime accepted network update: %d %s", resp.StatusCode, data)
	}
}
