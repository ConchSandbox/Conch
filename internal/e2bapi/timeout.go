package e2bapi

import (
	"context"
	"math"
	"net/http"
	"time"

	"github.com/openeuler/Conch/internal/sandbox"
)

type setTimeoutRequest struct {
	Timeout *int64 `json:"timeout"`
}

type refreshRequest struct {
	Duration int64 `json:"duration"`
}

// refreshMinSeconds is the floor native applies to refresh durations.
const refreshMinSeconds = 15

// maxTTLSeconds caps request TTLs at what an int64 nanosecond duration holds.
const maxTTLSeconds = math.MaxInt64 / int64(time.Second)

// setTimeout resets the sandbox TTL deadline to now+timeout. Unlike refreshes
// and connect it may shorten the remaining lifetime; zero and negative values
// expire the sandbox now. There is no request-level ceiling: the
// total-lifetime cap clamps the deadline instead. The timeout field is
// required: a missing field or JSON null is rejected with 400.
func (s *Server) setTimeout(w http.ResponseWriter, r *http.Request, id string) {
	var request *setTimeoutRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if request == nil {
		writeError(w, http.StatusBadRequest, "request body must be an object")
		return
	}
	if request.Timeout == nil {
		writeError(w, http.StatusBadRequest, "timeout is required")
		return
	}
	seconds := *request.Timeout
	if seconds < 0 {
		seconds = 0
	}
	if seconds > maxTTLSeconds {
		seconds = maxTTLSeconds
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.config.RequestTimeout)
	defer cancel()
	if _, err := s.runtime.ApplySandboxTTL(ctx, id, sandbox.TTLRequest{
		TTL: time.Duration(seconds) * time.Second, MaxLifetime: s.config.MaxSandboxTTL,
	}); err != nil {
		s.runtimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refreshes extends the TTL deadline to now+duration, never shortening it.
// The duration is floored at 15 seconds; requests beyond the total-lifetime
// cap extend only up to the cap.
func (s *Server) refresh(w http.ResponseWriter, r *http.Request, id string) {
	var request refreshRequest
	if !decodeBody(w, r, &request) {
		return
	}
	duration := request.Duration
	if duration < refreshMinSeconds {
		duration = refreshMinSeconds
	}
	if duration > maxTTLSeconds {
		duration = maxTTLSeconds
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.config.RequestTimeout)
	defer cancel()
	if _, err := s.runtime.ApplySandboxTTL(ctx, id, sandbox.TTLRequest{
		TTL: time.Duration(duration) * time.Second, ExtendOnly: true, MaxLifetime: s.config.MaxSandboxTTL,
	}); err != nil {
		s.runtimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
