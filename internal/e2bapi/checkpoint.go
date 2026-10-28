package e2bapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/vmm/driver"
)

type checkpointRequest struct {
	Name *string `json:"name"`
}

type checkpointResponse struct {
	SnapshotID string   `json:"snapshotID"`
	Names      []string `json:"names"`
}

// checkpoint publishes a Node-local resumable template. snapshotID is its
// mutable name, not its content digest; repeated named captures update the same
// template. This does not implement E2B pause/resume or distribute the template.
func (s *Server) checkpoint(w http.ResponseWriter, r *http.Request, id string) {
	var request *checkpointRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if request == nil {
		writeError(w, http.StatusBadRequest, "request body must be an object")
		return
	}
	name := "checkpoint-" + uuid.NewString()
	names := []string{}
	if request.Name != nil {
		name = strings.TrimSpace(*request.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "name must not be empty")
			return
		}
		// The create endpoint interprets valid digests as content IDs, so such
		// a name would not resolve back to this template.
		if _, err := digest.Parse(name); err == nil {
			writeError(w, http.StatusBadRequest, "name must not be a content digest")
			return
		}
		names = append(names, name)
	}
	// The capture can outlast the request budget, and the Manager-side max cannot extend this wrap.
	ctx, cancel := context.WithTimeout(r.Context(), max(s.config.RequestTimeout, driver.SnapshotCaptureTimeout))
	defer cancel()
	rec, err := s.runtime.GetSandbox(ctx, id)
	if err != nil {
		s.runtimeError(r.Context(), w, err)
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
	opts := conchruntime.SandboxCheckpointOptions{
		SandboxID: rec.ID, TemplateName: name,
	}
	if request.Name != nil {
		// The alias label lets the snapshot list report the client-visible
		// name; auto-named captures list without one.
		opts.Labels = map[string]string{conchruntime.SnapshotAliasLabel: name}
	}
	if _, err := s.runtime.CheckpointSandbox(ctx, opts); err != nil {
		s.runtimeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusCreated, checkpointResponse{SnapshotID: name, Names: names})
}
