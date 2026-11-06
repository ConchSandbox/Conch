package sandboxproxy

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/openeuler/Conch/pkg/requestid"
)

func TestProxyForwardsValidatedRequestID(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Upstream metadata must not introduce a second response identity.
		w.Header().Set(requestid.Header, "guest-local-request")
		_ = json.NewEncoder(w).Encode(r.Header.Values(requestid.Header))
	}))
	defer guest.Close()
	u, err := url.Parse(guest.URL)
	if err != nil {
		t.Fatal(err)
	}
	ip, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	defer registry.Close()
	id := uuid.NewString()
	registry.Insert(id)
	if err := registry.Publish(id, ip, ""); err != nil {
		t.Fatal(err)
	}
	handler := requestid.Middleware(NewHandler(registry, nil, nil))
	for _, tc := range []struct {
		name     string
		values   []string
		hopByHop bool
	}{
		{name: "provided", values: []string{"proxy-request-206"}},
		{name: "missing"},
		{name: "invalid", values: []string{"invalid id"}},
		{name: "multiple", values: []string{"first", "second"}},
		{name: "duplicate", values: []string{"duplicate", "duplicate"}},
		{name: "connection header", values: []string{"hop-request-206"}, hopByHop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://node/proxy/health", nil)
			r.Header.Set(E2BSandboxIDHeader, id)
			r.Header.Set(E2BTargetPortHeader, port)
			for _, value := range tc.values {
				r.Header.Add(requestid.Header, value)
			}
			if tc.hopByHop {
				r.Header.Set("Connection", requestid.Header)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("proxy status = %d: %s", w.Code, w.Body.String())
			}
			responseIDs := w.Header().Values(requestid.Header)
			if len(responseIDs) != 1 || !requestid.Valid(responseIDs[0]) {
				t.Fatalf("response IDs = %q", responseIDs)
			}
			if len(tc.values) == 1 && requestid.Valid(tc.values[0]) && responseIDs[0] != tc.values[0] {
				t.Fatalf("caller ID changed: %q", responseIDs)
			}
			var forwarded []string
			if err := json.Unmarshal(w.Body.Bytes(), &forwarded); err != nil {
				t.Fatal(err)
			}
			if len(forwarded) != 1 || forwarded[0] != responseIDs[0] {
				t.Fatalf("forwarded IDs = %q, response IDs = %q", forwarded, responseIDs)
			}
		})
	}
}
