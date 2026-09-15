package e2bapi

import (
	"context"
	"net/http"
	"time"
)

type setTimeoutRequest struct {
	Timeout int64 `json:"timeout"`
}

type refreshRequest struct {
	Duration int64 `json:"duration"`
}

const (
	refreshMinSeconds = 15
	refreshMaxSeconds = 3600
)

// set_timeout resets the E2B TTL deadline to now+timeout. Unlike refreshes it
// may shorten the remaining lifetime, matching the SDK contract.
func (s *Server) setTimeout(w http.ResponseWriter, r *http.Request, id string) {
	var request *setTimeoutRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if request.Timeout < 1 || request.Timeout > 86400 {
		writeError(w, http.StatusBadRequest, "timeout must be between 1 and 86400 seconds")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.config.RequestTimeout)
	defer cancel()
	if _, err := s.runtime.ApplySandboxTTL(ctx, id, time.Duration(request.Timeout)*time.Second, false); err != nil {
		s.runtimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refreshes extends the E2B TTL by duration seconds. It never shortens the
// deadline, and the requested duration is clamped to [15, 3600] seconds.
func (s *Server) refresh(w http.ResponseWriter, r *http.Request, id string) {
	var request refreshRequest
	if !decodeBody(w, r, &request) {
		return
	}
	duration := request.Duration
	if duration == 0 {
		duration = refreshMinSeconds
	}
	if duration < refreshMinSeconds {
		duration = refreshMinSeconds
	}
	if duration > refreshMaxSeconds {
		duration = refreshMaxSeconds
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.config.RequestTimeout)
	defer cancel()
	if _, err := s.runtime.ApplySandboxTTL(ctx, id, time.Duration(duration)*time.Second, true); err != nil {
		s.runtimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
