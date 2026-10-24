// Package e2bapi adapts Conch's local runtime to the AgentENV Node HTTP
// contract. It is a separate TCP facade; guest data is routed by sandboxproxy.
package e2bapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
	"github.com/openeuler/Conch/internal/apperror"
	"github.com/openeuler/Conch/internal/conchruntime"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/sandboxproxy"
	"github.com/openeuler/Conch/internal/vmm/driver"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	maxBodyBytes     = 1 << 20
	resumePollPeriod = 200 * time.Millisecond
)

// connectWait bounds awaitResume's poll; a var so tests can shorten the wait.
var connectWait = 60 * time.Second

type Config struct {
	APIKey         string
	Domains        []string
	RequestTimeout time.Duration
	MaxSandboxTTL  time.Duration
}

type Server struct {
	runtime *conchruntime.Service
	proxy   *sandboxproxy.Handler
	config  Config
}

func New(cfg Config, service *conchruntime.Service, routes *sandboxproxy.Registry) (*Server, error) {
	if cfg.MaxSandboxTTL < time.Second {
		return nil, fmt.Errorf("E2B API requires a max sandbox TTL of at least 1s")
	}
	if cfg.APIKey == "" || cfg.RequestTimeout <= 0 || service == nil || service.Store == nil || routes == nil {
		return nil, fmt.Errorf("E2B API requires an API key, request timeout, runtime/store and proxy registry")
	}
	// A wake restores a paused sandbox with the upstream minimum TTL floor,
	// capped by the operator's TTL ceiling.
	wakeTTL := autoResumeMinTTL
	if cfg.MaxSandboxTTL < wakeTTL {
		wakeTTL = cfg.MaxSandboxTTL
	}
	wake := func(ctx context.Context, sandboxID string) error {
		return service.EnsureSandboxRunning(ctx, sandboxID, wakeTTL)
	}
	return &Server{runtime: service, proxy: sandboxproxy.NewHandler(routes, cfg.Domains, wake), config: cfg}, nil
}

func (s *Server) ttlRangeMessage() string {
	return fmt.Sprintf("timeout must be between 1 and %d seconds", int64(s.config.MaxSandboxTTL/time.Second))
}

// matchAPIKey compares SHA-256 digests so a length mismatch cannot return early.
func matchAPIKey(provided, configured string) bool {
	providedSum := sha256.Sum256([]byte(provided))
	configuredSum := sha256.Sum256([]byte(configured))
	return subtle.ConstantTimeCompare(providedSum[:], configuredSum[:]) == 1
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Host routing takes precedence even when an application uses an API path.
	if s.proxy.IsHostRoute(r) {
		s.proxy.ServeHTTP(w, r)
		return
	}
	path := strings.TrimRight(r.URL.Path, "/")
	control := path == "/sandboxes" || path == "/v2/sandboxes" || strings.HasPrefix(path, "/sandboxes/") || path == "/sandboxes-cold" || path == "/snapshots"
	if !control && s.proxy.IsProxyRequest(r) {
		s.proxy.ServeHTTP(w, r)
		return
	}
	if path == "/health" && r.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		return
	}
	keys := r.Header.Values("X-API-Key")
	if len(keys) != 1 || !matchAPIKey(keys[0], s.config.APIKey) {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	switch {
	case path == "/sandboxes" && r.Method == http.MethodPost:
		s.create(w, r)
	case (path == "/sandboxes" || path == "/v2/sandboxes") && r.Method == http.MethodGet:
		s.list(w, r)
	case path == "/snapshots" && r.Method == http.MethodGet:
		s.listSnapshots(w, r)
	case strings.HasPrefix(path, "/sandboxes/"):
		parts := strings.Split(strings.TrimPrefix(path, "/sandboxes/"), "/")
		if len(parts) == 1 {
			switch r.Method {
			case http.MethodGet:
				s.get(w, r, parts[0])
			case http.MethodDelete:
				s.kill(w, r, parts[0])
			default:
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			}
			return
		}
		if len(parts) == 2 {
			switch parts[1] + " " + r.Method {
			case "pause POST":
				s.pause(w, r, parts[0])
			case "connect POST":
				s.connect(w, r, parts[0])
			case "resume POST", "fork POST":
				// E2B resume and fork would start a new sandbox from a
				// checkpoint template; connect already restores paused ones.
				unimplemented(w, parts[1])
			case "snapshots POST":
				s.checkpoint(w, r, parts[0])
			case "timeout POST":
				s.setTimeout(w, r, parts[0])
			case "refreshes POST":
				s.refresh(w, r, parts[0])
			case "network PUT":
				s.updateNetwork(w, r, parts[0])
			case "metrics GET":
				unimplemented(w, "sandbox metrics")
			default:
				writeError(w, http.StatusNotFound, "endpoint not found")
			}
			return
		}
		unimplemented(w, "sandbox operation")
	case path == "/sandboxes-cold" || strings.HasPrefix(path, "/templates") || strings.HasPrefix(path, "/volumes"):
		unimplemented(w, "cold creation, template and volume APIs")
	default:
		writeError(w, http.StatusNotFound, "endpoint not found")
	}
}

type networkRequest struct {
	AllowPublicTraffic *bool             `json:"allowPublicTraffic"`
	AllowOut           []string          `json:"allowOut"`
	DenyOut            []string          `json:"denyOut"`
	EgressProxy        json.RawMessage   `json:"egressProxy"`
	Rules              []json.RawMessage `json:"rules"`
	MaskRequestHost    string            `json:"maskRequestHost"`
}

type createRequest struct {
	TemplateID          string            `json:"templateID"`
	Timeout             *int64            `json:"timeout"`
	Secure              *bool             `json:"secure"`
	Metadata            map[string]string `json:"metadata"`
	EnvVars             map[string]string `json:"envVars"`
	AllowInternetAccess *bool             `json:"allow_internet_access"`
	Network             *networkRequest   `json:"network"`
	AutoPause           *bool             `json:"autoPause"`
	AutoPauseMemory     *bool             `json:"autoPauseMemory"`
	AutoResume          *struct {
		Enabled bool `json:"enabled"`
	} `json:"autoResume"`
	VolumeMounts          []json.RawMessage `json:"volumeMounts"`
	MCP                   json.RawMessage   `json:"mcp"`
	CustomExtensionParams json.RawMessage   `json:"customExtensionParams"`
}

func decodeBody(w http.ResponseWriter, r *http.Request, value any) bool {
	reader := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer reader.Close()
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(value)
	if err == nil {
		var extra any
		if next := decoder.Decode(&extra); next != io.EOF {
			err = fmt.Errorf("expected one JSON object")
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		}
		return false
	}
	return true
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var request createRequest
	if !decodeBody(w, r, &request) {
		return
	}
	request.TemplateID = strings.TrimSpace(request.TemplateID)
	if request.TemplateID == "" {
		writeError(w, 400, "templateID is required")
		return
	}
	if request.Secure == nil || *request.Secure {
		unimplemented(w, "secure envd; explicitly set secure=false")
		return
	}
	if request.AutoPauseMemory != nil && !*request.AutoPauseMemory {
		writeError(w, 400, "autoPauseMemory=false (disk-only pause) is not supported")
		return
	}
	if len(request.VolumeMounts) > 0 || nonemptyJSON(request.MCP) || nonemptyJSON(request.CustomExtensionParams) {
		unimplemented(w, "volume mounts, MCP and custom extensions")
		return
	}
	ttl := int64(300)
	if request.Timeout != nil {
		ttl = *request.Timeout
	}
	if ttl < 1 || ttl > int64(s.config.MaxSandboxTTL/time.Second) {
		writeError(w, 400, s.ttlRangeMessage())
		return
	}
	opts := runtimeapi.SandboxCreateOptions{
		SandboxID: uuid.NewString(), E2B: true, Env: request.EnvVars, Metadata: request.Metadata, Timeout: time.Duration(ttl) * time.Second,
		TimeoutAction: runtimeapi.TimeoutActionDelete,
		AutoResume:    request.AutoResume != nil && request.AutoResume.Enabled,
	}
	if request.AutoPause != nil && *request.AutoPause {
		opts.TimeoutAction = runtimeapi.TimeoutActionPause
	}
	if parsed, err := digest.Parse(request.TemplateID); err == nil {
		opts.TemplateID = parsed.String()
	} else {
		opts.TemplateName = request.TemplateID
	}
	if request.Network != nil {
		if request.Network.AllowPublicTraffic != nil && !*request.Network.AllowPublicTraffic {
			unimplemented(w, "private traffic")
			return
		}
		if nonemptyJSON(request.Network.EgressProxy) || len(request.Network.Rules) > 0 {
			unimplemented(w, "network egressProxy and rules")
			return
		}
		opts.MaskRequestHost = request.Network.MaskRequestHost
		opts.Network = &runtimeapi.SandboxNetworkConfig{AllowOut: request.Network.AllowOut, DenyOut: request.Network.DenyOut, AllowInternetAccess: request.AllowInternetAccess}
	} else if request.AllowInternetAccess != nil {
		opts.Network = &runtimeapi.SandboxNetworkConfig{AllowInternetAccess: request.AllowInternetAccess}
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.config.RequestTimeout)
	defer cancel()
	created, err := s.runtime.CreateSandbox(ctx, opts)
	if err != nil {
		s.runtimeError(w, err)
		return
	}
	// Build the creation response from its committed result.
	w.Header().Set(sandboxproxy.SandboxIDHeader, created.SandboxID)
	writeJSON(w, http.StatusCreated, s.model(sandbox.Record{
		ID: created.SandboxID, SourceTemplateID: created.TemplateID,
		SourceTemplateName: created.TemplateName, EnvdVersion: created.EnvdVersion,
	}))
}

func nonemptyJSON(raw json.RawMessage) bool {
	v := strings.TrimSpace(string(raw))
	return v != "" && v != "null" && v != "{}"
}

type sandboxModel struct {
	TemplateID  string  `json:"templateID"`
	SandboxID   string  `json:"sandboxID"`
	ClientID    string  `json:"clientID"`
	EnvdVersion string  `json:"envdVersion"`
	Alias       string  `json:"alias,omitempty"`
	Domain      *string `json:"domain,omitempty"`
}

type sandboxLifecycle struct {
	AutoResume bool   `json:"autoResume"`
	OnTimeout  string `json:"onTimeout"`
}

type sandboxDetail struct {
	sandboxModel
	StartedAt  time.Time         `json:"startedAt"`
	EndAt      time.Time         `json:"endAt"`
	CPUCount   int64             `json:"cpuCount"`
	MemoryMB   int64             `json:"memoryMB"`
	DiskSizeMB int64             `json:"diskSizeMB"`
	State      string            `json:"state"`
	Metadata   map[string]string `json:"metadata"`
	Lifecycle  *sandboxLifecycle `json:"lifecycle,omitempty"`
}

func (s *Server) model(rec sandbox.Record) sandboxModel {
	result := sandboxModel{TemplateID: rec.SourceTemplateID, SandboxID: rec.ID, ClientID: rec.ID, EnvdVersion: rec.EnvdVersion, Alias: rec.SourceTemplateName}
	if len(s.config.Domains) > 0 {
		domain := s.config.Domains[0]
		result.Domain = &domain
	}
	return result
}

// pausedView reports whether the record answers externally as paused: a
// PAUSED record, or a checkpoint lineage stranded in CREATING or UNKNOWN —
// an in-place resume in flight or one whose terminal write failed — which
// ResumePaused accepts as the same paused sandbox.
func pausedView(rec sandbox.Record) bool {
	return rec.State == sandbox.StatePaused || sandbox.StrandedResume(rec)
}

// runStartedAt reports the beginning of the run the current view shows: the
// checkpoint-producing run for a paused view, StartedAt for a running one,
// CreatedAt for legacy records without either.
func runStartedAt(rec sandbox.Record) time.Time {
	started := rec.StartedAt
	if pausedView(rec) && rec.HeadStartedAt > 0 {
		started = rec.HeadStartedAt
	}
	if started <= 0 {
		started = rec.CreatedAt
	}
	return time.Unix(0, started).UTC()
}

// viewEndAt reports the end of the current view: the expiry of the running
// sandbox, or the pause time captured in the checkpoint head.
func viewEndAt(rec sandbox.Record) time.Time {
	if pausedView(rec) {
		end := rec.HeadCreatedAt
		if end <= 0 {
			end = rec.CreatedAt
		}
		return time.Unix(0, end).UTC()
	}
	return time.Unix(0, rec.ExpiresAt).UTC()
}

func (s *Server) detail(rec sandbox.Record) sandboxDetail {
	metadata := rec.Metadata
	if metadata == nil {
		metadata = map[string]string{}
	}
	state := "running"
	if pausedView(rec) {
		state = "paused"
	}
	// E2B's onTimeout vocabulary is kill/pause; Conch's delete action is the
	// upstream kill, and an unset action keeps the same default semantics.
	onTimeout := "kill"
	if rec.TimeoutAction == runtimeapi.TimeoutActionPause {
		onTimeout = "pause"
	}
	return sandboxDetail{sandboxModel: s.model(rec), StartedAt: runStartedAt(rec), EndAt: viewEndAt(rec),
		CPUCount: rec.VCPUNum, MemoryMB: rec.RamMB, State: state, Metadata: metadata,
		Lifecycle: &sandboxLifecycle{AutoResume: rec.AutoResume, OnTimeout: onTimeout}}
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, id string) {
	rec, err := s.runtime.GetSandbox(r.Context(), id)
	if err != nil {
		s.runtimeError(w, err)
		return
	}
	if !rec.E2B {
		writeError(w, 404, "sandbox not found")
		return
	}
	switch {
	case rec.State == sandbox.StateReady || pausedView(rec):
		writeJSON(w, 200, s.detail(rec))
	case rec.State == sandbox.StateCreating:
		// A fresh create has no snapshot row until bootstrap completes: the
		// sandbox does not exist externally yet.
		writeError(w, 404, "sandbox not found")
	default:
		writeError(w, 409, "sandbox is not running")
	}
}

func (s *Server) kill(w http.ResponseWriter, r *http.Request, id string) {
	rec, err := s.runtime.GetSandbox(r.Context(), id)
	if err != nil {
		s.runtimeError(w, err)
		return
	}
	if !rec.E2B {
		writeError(w, 404, "sandbox not found")
		return
	}
	if err := s.runtime.RemoveSandbox(r.Context(), id); err != nil {
		s.runtimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type pauseRequest struct {
	Memory *bool `json:"memory"`
}

type connectRequest struct {
	Timeout *int64 `json:"timeout"`
}

// pause captures the sandbox's full memory state into a checkpoint and
// releases its runtime resources. The record survives as PAUSED with no
// expiry, holding the checkpoint head produced by the checkpoint pipeline.
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
	// The capture can outlast the request budget, and the Manager-side max cannot extend this wrap.
	ctx, cancel := context.WithTimeout(r.Context(), max(s.config.RequestTimeout, driver.SnapshotCaptureTimeout))
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
	// The runtime re-checks the state under the lifecycle lock, so a pause
	// racing a concurrent transition answers the lock-held verdict.
	if err := s.runtime.PauseSandbox(ctx, rec.ID, time.Time{}); err != nil {
		s.runtimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// connect implements the E2B connect semantics: a running sandbox only has
// its TTL extended (200), while a paused sandbox — including a checkpoint
// lineage stranded in UNKNOWN by a failed terminal write — is restored under
// the same ID from its checkpoint head (201). A lineage stranded in CREATING
// is a restore already in flight, so connect only waits for it. An absent or
// non-positive timeout leaves a running deadline untouched and starts a
// 300s run when resuming. A connect during an in-place resume waits for the
// running restore and then applies the READY semantics; the wait runs on its
// own connectWait budget and the resume after it on the runtime's operation
// timeout, so an expired request deadline answers 409 or 201 instead of
// 504 — only a client disconnect (410) interrupts either.
func (s *Server) connect(w http.ResponseWriter, r *http.Request, id string) {
	var request *connectRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if request == nil {
		writeError(w, http.StatusBadRequest, "request body must be an object")
		return
	}
	ttl := int64(0)
	if request.Timeout != nil {
		ttl = *request.Timeout
	}
	// The entry cap applies to both states before dispatch.
	if ttl > int64(s.config.MaxSandboxTTL/time.Second) {
		writeError(w, http.StatusBadRequest, s.ttlRangeMessage())
		return
	}
	// A negative wraps when scaled to a Duration: treat it as absent, and clamp the rest to the Duration ceiling.
	if ttl < 0 {
		ttl = 0
	}
	if ttl > maxTTLSeconds {
		ttl = maxTTLSeconds
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.config.RequestTimeout)
	defer cancel()
	resumeCtx, resumeCancel := resumeContext(ctx)
	defer resumeCancel()
	rec, err := s.runtime.GetSandbox(ctx, id)
	if err != nil {
		s.runtimeError(w, err)
		return
	}
	if !rec.E2B {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	// The timeout is the lifetime of the new run; an absent or non-positive
	// value uses the 300s default, capped by the ceiling.
	timeout := time.Duration(ttl) * time.Second
	if timeout <= 0 {
		timeout = min(300*time.Second, s.config.MaxSandboxTTL)
	}
	// A stranded lineage — UNKNOWN or CREATING with a checkpoint head past
	// the source — converges on waiting for a resume. Only the UNKNOWN one
	// kicks ResumePaused itself: its runtime entry is gone, so nothing blocks
	// the retry, and ErrAlreadyExists means another request is resuming. A
	// CREATING lineage is the create kernel's own in-flight state, so a kick
	// could only queue on the lifecycle lock; this request waits instead.
	// The wait also bounds the double-fault orphan — a CREATING lineage whose
	// entry died with its failed create, so nothing is resuming: connect ends
	// at a 409 rather than reviving it in-process. An autoResume orphan stays
	// wakeable from the data plane; the rest wait for an explicit delete or
	// a restart.
	if pausedView(rec) && rec.State != sandbox.StatePaused {
		if rec.State == sandbox.StateUnknown {
			if _, err := s.runtime.ResumePausedSandbox(ctx, rec.ID, sandbox.ResumePausedOptions{Timeout: timeout}); err == nil {
				restored, err := s.runtime.GetSandbox(ctx, id)
				if err != nil {
					s.runtimeError(w, err)
					return
				}
				writeJSON(w, http.StatusCreated, s.detail(restored))
				return
			} else if !errors.Is(err, sandbox.ErrAlreadyExists) {
				s.runtimeError(w, err)
				return
			}
		}
		restored, err := s.awaitResume(resumeCtx, id)
		if err != nil {
			switch {
			case errors.Is(err, errStillResuming):
				writeError(w, http.StatusConflict, "sandbox is still resuming")
			case errors.Is(err, context.Canceled):
				writeError(w, http.StatusGone, "sandbox request was canceled")
			default:
				s.runtimeError(w, err)
			}
			return
		}
		if restored.State == sandbox.StateReady {
			// The concurrent resume finished: answer as the joined request,
			// 201 without touching the winner's deadline (upstream waitForStart).
			writeJSON(w, http.StatusCreated, s.detail(restored))
			return
		}
		rec = restored
	}
	// A READY sandbox — including one whose runtime crashed — is externally
	// running, so connect only moves its deadline.
	if rec.State == sandbox.StateReady {
		updated, err := s.runtime.ApplySandboxTTL(ctx, rec.ID, sandbox.TTLRequest{
			TTL: time.Duration(ttl) * time.Second, ExtendOnly: true, MaxLifetime: s.config.MaxSandboxTTL,
		})
		if err == nil {
			writeJSON(w, http.StatusOK, s.detail(updated))
			return
		}
		// A failed TTL write re-reads the record: one flipped to PAUSED by a
		// concurrent pause between the two reads continues as the paused case
		// below instead of surfacing the stale precondition.
		refreshed, getErr := s.runtime.GetSandbox(ctx, id)
		if getErr != nil || !pausedView(refreshed) {
			s.runtimeError(w, err)
			return
		}
		rec = refreshed
	}
	switch {
	case pausedView(rec):
		// A PAUSED record — or a concurrent resume that failed while this
		// request waited and left the record PAUSED again. A loser of the
		// resume race (both reads saw PAUSED) succeeds as the joined request
		// and returns 201 with the winner's run.
		if _, err := s.runtime.ResumePausedSandbox(resumeCtx, rec.ID, sandbox.ResumePausedOptions{Timeout: timeout}); err != nil {
			s.runtimeError(w, err)
			return
		}
		restored, err := s.runtime.GetSandbox(resumeCtx, id)
		if err != nil {
			s.runtimeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, s.detail(restored))
	case rec.State == sandbox.StateCreating:
		// A fresh create has no snapshot row until bootstrap completes.
		writeError(w, http.StatusNotFound, "sandbox not found")
	default:
		writeError(w, http.StatusNotFound, "sandbox is shutting down")
	}
}

// errStillResuming reports a connect wait whose budget ran out while the
// record was still CREATING.
var errStillResuming = errors.New("sandbox is still resuming")

// resumeContext strips the request deadline while keeping client
// cancellation: the connect wait and the resume it hands off to run on
// their own budgets — connectWait and the runtime's operation timeout —
// not on the remainder of a request that may have expired waiting.
func resumeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	resumeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if ctx.Done() == nil {
		return resumeCtx, cancel
	}
	go func() {
		select {
		case <-ctx.Done():
			// Only a client disconnect propagates: a request deadline must
			// not cut the wait short.
			if ctx.Err() == context.Canceled {
				cancel()
			}
		case <-resumeCtx.Done():
		}
	}()
	return resumeCtx, cancel
}

// awaitResume polls an in-place resume until the record leaves the CREATING
// state. The budget mirrors bootstrapTimeout, the span the resume kernel can
// hold the lifecycle lock for. The caller passes a context stripped of the
// request deadline, so the budget belongs to connectWait alone; read
// failures and the context verdict are returned verbatim, and
// errStillResuming marks the exhausted budget.
func (s *Server) awaitResume(ctx context.Context, id string) (sandbox.Record, error) {
	deadline := time.Now().Add(connectWait)
	for {
		rec, err := s.runtime.GetSandbox(ctx, id)
		if err != nil {
			return sandbox.Record{}, err
		}
		if rec.State != sandbox.StateCreating {
			return rec, nil
		}
		if time.Now().After(deadline) {
			return sandbox.Record{}, errStillResuming
		}
		select {
		case <-ctx.Done():
			return sandbox.Record{}, ctx.Err()
		case <-time.After(resumePollPeriod):
		}
	}
}

// RunExpiry enforces sandbox TTLs. A timeout on a live TimeoutAction=pause
// sandbox checkpoints and pauses it instead of deleting it; a pause dispatch
// whose runtime already died falls back to the expire terminal, which always
// deletes. Both dispatches revalidate the deadline under the sandbox's
// lifecycle lock, so a TTL extension racing with this tick wins. Failures
// are retried on the next tick; UNKNOWN records are left to an explicit
// delete or startup recovery.
func (s *Server) RunExpiry(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			records, err := s.runtime.ListSandboxes(ctx)
			if err != nil {
				if ctx.Err() == nil {
					ulog.Warn("list expired E2B sandboxes", ulog.F("error", err))
				}
				continue
			}
			for _, rec := range records {
				switch {
				case rec.E2B && rec.State == sandbox.StateReady && rec.ExpiresAt > 0 && rec.ExpiresAt <= now.UnixNano():
					if rec.TimeoutAction == runtimeapi.TimeoutActionPause {
						if err := s.runtime.PauseSandbox(ctx, rec.ID, now); err != nil {
							// A dead runtime has nothing left to checkpoint:
							// finish it through the delete-only expire terminal.
							if errors.Is(err, sandbox.ErrRuntimeDead) {
								if expireErr := s.runtime.ExpireSandbox(ctx, rec.ID, now); expireErr != nil {
									ulog.Warn("expire dead E2B sandbox", ulog.F("sandbox_id", rec.ID), ulog.F("error", expireErr))
								}
							} else {
								ulog.Warn("auto-pause expired E2B sandbox", ulog.F("sandbox_id", rec.ID), ulog.F("error", err))
							}
						}
						continue
					}
					if err := s.runtime.ExpireSandbox(ctx, rec.ID, now); err != nil {
						ulog.Warn("expire E2B sandbox", ulog.F("sandbox_id", rec.ID), ulog.F("error", err))
					}
				}
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{status, message})
}

func unimplemented(w http.ResponseWriter, feature string) {
	writeError(w, 501, "Unimplemented: "+feature+" is not supported in this phase")
}

func (s *Server) runtimeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "internal server error"
	var app *apperror.Error
	if errors.As(err, &app) {
		message = app.PublicMessage()
		switch app.Kind() {
		case apperror.InvalidArgument:
			status = 400
		case apperror.NotFound:
			status = 404
		case apperror.AlreadyExists, apperror.Conflict, apperror.FailedPrecondition:
			status = 409
		case apperror.ResourceExhausted:
			status = 429
		case apperror.NotImplemented:
			status = 501
		case apperror.Unavailable:
			status = 503
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		status = 504
		message = "sandbox operation timed out"
	}
	if status >= 500 {
		ulog.Warn("E2B operation failed", ulog.F("error", err))
	}
	writeError(w, status, message)
}
