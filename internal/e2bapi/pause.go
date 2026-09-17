package e2bapi

import (
	"context"
	"net/http"
	"time"

	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
)

type pauseRequest struct {
	// Memory selects a full-state pause. Only true is supported; a false
	// value would need a disk-only snapshot, which Conch cannot produce.
	Memory *bool `json:"memory"`
}

type connectRequest struct {
	Timeout *int64 `json:"timeout"`
}

// pause captures the sandbox's full memory state into a resume template and
// releases its runtime resources. The record survives as PAUSED with no
// expiry, holding only the resume template produced by the checkpoint
// pipeline.
func (s *Server) pause(w http.ResponseWriter, r *http.Request, id string) {
	var request *pauseRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if request == nil {
		writeError(w, http.StatusBadRequest, "request body must be an object")
		return
	}
	if request.Memory != nil && !*request.Memory {
		writeError(w, http.StatusBadRequest, "memory=false (disk-only pause) is not supported")
		return
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
	if rec.State == sandbox.StatePaused {
		writeError(w, http.StatusConflict, "sandbox is already paused")
		return
	}
	if rec.State != sandbox.StateReady {
		writeError(w, http.StatusConflict, "sandbox is not running")
		return
	}
	// This fork marks an unconfirmed cleanup by retaining the record as
	// UNKNOWN instead of a cleanup-pending flag.
	if rec.State == sandbox.StateUnknown {
		writeError(w, http.StatusNotFound, "sandbox is shutting down")
		return
	}
	if len(rec.VolumeMounts) > 0 {
		writeError(w, http.StatusConflict, "sandbox has volume mounts, pause is not supported")
		return
	}
	if err := s.runtime.PauseSandbox(ctx, rec.ID); err != nil {
		s.runtimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// connect implements the E2B connect semantics: a running sandbox only has
// its TTL extended (200), while a paused sandbox is restored under the same
// ID from its resume template (201).
func (s *Server) connect(w http.ResponseWriter, r *http.Request, id string) {
	var request *connectRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if request == nil {
		writeError(w, http.StatusBadRequest, "request body must be an object")
		return
	}
	ttl := int64(300)
	if request.Timeout != nil {
		ttl = *request.Timeout
	}
	if ttl < 1 || ttl > 86400 {
		writeError(w, http.StatusBadRequest, "timeout must be between 1 and 86400 seconds")
		return
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
	switch rec.State {
	case sandbox.StateReady:
		updated, err := s.runtime.ApplySandboxTTL(ctx, rec.ID, time.Duration(ttl)*time.Second, false)
		if err != nil {
			s.runtimeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.detail(updated))
	case sandbox.StatePaused:
		if _, err := s.runtime.ResumePausedSandbox(ctx, rec.ID, runtimeapi.SandboxResumeOptions{
			Timeout: time.Duration(ttl) * time.Second,
		}); err != nil {
			s.runtimeError(w, err)
			return
		}
		restored, err := s.runtime.GetSandbox(ctx, id)
		if err != nil {
			s.runtimeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, s.detail(restored))
	case sandbox.StateCreating:
		writeError(w, http.StatusConflict, "sandbox is still creating")
	default:
		writeError(w, http.StatusNotFound, "sandbox is shutting down")
	}
}
