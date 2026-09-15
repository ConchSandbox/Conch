package e2bapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
)

// updateNetworkRequest mirrors the SDK's update_network payload keys. The
// egress proxy and per-port rules are not supported by the Conch netstack and
// are rejected explicitly.
type updateNetworkRequest struct {
	AllowOut            []string          `json:"allowOut"`
	DenyOut             []string          `json:"denyOut"`
	EgressProxy         json.RawMessage   `json:"egressProxy"`
	Rules               []json.RawMessage `json:"rules"`
	AllowInternetAccess *bool             `json:"allow_internet_access"`
}

// updateNetwork replaces the egress policy of a running sandbox. The payload
// is interpreted as a whole: allowInternetAccess=false is equivalent to
// denying all outbound traffic.
func (s *Server) updateNetwork(w http.ResponseWriter, r *http.Request, id string) {
	var request updateNetworkRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if nonemptyJSON(request.EgressProxy) || len(request.Rules) > 0 {
		writeError(w, http.StatusBadRequest, "egressProxy and rules are not supported")
		return
	}
	network := &runtimeapi.SandboxNetworkConfig{
		AllowOut: request.AllowOut,
		DenyOut:  request.DenyOut,
	}
	if request.AllowInternetAccess != nil {
		network.AllowInternetAccess = request.AllowInternetAccess
		if !*request.AllowInternetAccess {
			network.DenyOut = append([]string{"0.0.0.0/0"}, network.DenyOut...)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.config.RequestTimeout)
	defer cancel()
	rec, err := s.runtime.GetSandbox(ctx, id)
	if err != nil {
		s.runtimeError(w, err)
		return
	}
	if !rec.E2B {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if rec.State != sandbox.StateReady {
		writeError(w, http.StatusConflict, "sandbox is not running")
		return
	}
	if err := s.runtime.UpdateSandboxNetworkConfig(ctx, runtimeapi.SandboxNetworkUpdateOptions{
		SandboxID: rec.ID,
		Network:   network,
	}); err != nil {
		s.runtimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
