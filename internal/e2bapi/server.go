// Package e2bapi adapts Conch's local runtime to the AgentENV Node HTTP
// contract. It is a separate TCP facade; guest data is routed by sandboxproxy.
package e2bapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	"github.com/openeuler/Conch/pkg/ulog"
)

const maxBodyBytes = 1 << 20

type Config struct {
	APIKey         string
	Domains        []string
	RequestTimeout time.Duration
}

type Server struct {
	runtime *conchruntime.Service
	proxy   *sandboxproxy.Handler
	volumes volumeAPI
	config  Config
}

// SetVolumes wires the node-local volume registry used by the /volumes API and
// to resolve create-time volumeMounts entries into host-backed directories.
func (s *Server) SetVolumes(volumes volumeAPI) {
	if s != nil {
		s.volumes = volumes
	}
}

func New(cfg Config, service *conchruntime.Service, routes *sandboxproxy.Registry) (*Server, error) {
	if cfg.APIKey == "" || cfg.RequestTimeout <= 0 || service == nil || service.Store == nil || routes == nil {
		return nil, fmt.Errorf("E2B API requires an API key, request timeout, runtime/store and proxy registry")
	}
	return &Server{runtime: service, proxy: sandboxproxy.NewHandler(routes, cfg.Domains), config: cfg}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Host routing takes precedence even when an application uses an API path.
	if s.proxy.IsHostRoute(r) {
		s.proxy.ServeHTTP(w, r)
		return
	}
	path := strings.TrimRight(r.URL.Path, "/")
	control := path == "/sandboxes" || path == "/v2/sandboxes" || strings.HasPrefix(path, "/sandboxes/") || path == "/sandboxes-cold"
	if !control && s.proxy.IsProxyRequest(r) {
		s.proxy.ServeHTTP(w, r)
		return
	}
	if path == "/health" && r.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		return
	}
	keys := r.Header.Values("X-API-Key")
	if len(keys) != 1 || subtle.ConstantTimeCompare([]byte(keys[0]), []byte(s.config.APIKey)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	switch {
	case path == "/sandboxes" && r.Method == http.MethodPost:
		s.create(w, r)
	case (path == "/sandboxes" || path == "/v2/sandboxes") && r.Method == http.MethodGet:
		s.list(w, r)
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
			case "pause POST", "connect POST", "resume POST", "fork POST":
				// Conch's native resume only un-pauses a live VMM process; full
				// E2B pause/restore semantics are future work.
				unimplemented(w, parts[1])
			case "snapshots POST":
				s.checkpoint(w, r, parts[0])
			case "timeout POST", "refreshes POST":
				unimplemented(w, parts[1])
			case "network PUT":
				s.updateNetwork(w, r, parts[0])
			default:
				writeError(w, http.StatusNotFound, "endpoint not found")
			}
			return
		}
		unimplemented(w, "sandbox operation")
	case strings.HasPrefix(path, "/volumes"):
		s.volumeRoutes(w, r, strings.Split(strings.TrimPrefix(path, "/volumes"), "/"))
	case path == "/sandboxes-cold" || strings.HasPrefix(path, "/templates"):
		unimplemented(w, "cold creation and template APIs")
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

type volumeMountRequest struct {
	Name string `json:"name"`
	Path string `json:"path"`
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
	if (request.AutoPause != nil && *request.AutoPause) || (request.AutoResume != nil && request.AutoResume.Enabled) {
		// Full pause semantics (checkpoint, restore, connect) are ported with
		// the pause/TTL feature.
		unimplemented(w, "automatic pause/resume")
		return
	}
	if request.AutoPauseMemory != nil && !*request.AutoPauseMemory {
		writeError(w, 400, "autoPauseMemory=false (disk-only pause) is not supported")
		return
	}
	if nonemptyJSON(request.MCP) || nonemptyJSON(request.CustomExtensionParams) {
		unimplemented(w, "MCP and custom extensions")
		return
	}
	ttl := int64(300)
	if request.Timeout != nil {
		ttl = *request.Timeout
	}
	if ttl < 0 || ttl > math.MaxInt32 {
		writeError(w, 400, "timeout must be between 0 and 2147483647 seconds")
		return
	}
	opts := runtimeapi.SandboxCreateOptions{
		SandboxID: uuid.NewString(), E2B: true, Env: request.EnvVars, Metadata: request.Metadata, Timeout: time.Duration(ttl) * time.Second,
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
		network := &runtimeapi.SandboxNetworkConfig{
			AllowOut: request.Network.AllowOut, DenyOut: request.Network.DenyOut,
			AllowInternetAccess: request.AllowInternetAccess,
		}
		opts.MaskRequestHost = request.Network.MaskRequestHost
		opts.Network = network
	} else if request.AllowInternetAccess != nil {
		opts.Network = &runtimeapi.SandboxNetworkConfig{AllowInternetAccess: request.AllowInternetAccess}
	}
	var volumeMounts []runtimeapi.VolumeMountSpec
	for _, raw := range request.VolumeMounts {
		var mount volumeMountRequest
		if err := json.Unmarshal(raw, &mount); err != nil || strings.TrimSpace(mount.Name) == "" || strings.TrimSpace(mount.Path) == "" {
			writeError(w, 400, "volumeMounts entries require name and path")
			return
		}
		volumeMounts = append(volumeMounts, runtimeapi.VolumeMountSpec{Name: mount.Name, Path: mount.Path})
	}
	if s.volumes != nil && len(volumeMounts) > 0 {
		resolved, err := s.volumes.ResolveMounts(r.Context(), volumeMounts)
		if err != nil {
			s.runtimeError(w, err)
			return
		}
		volumeMounts = resolved
	} else if len(volumeMounts) > 0 {
		writeError(w, 400, "volume mounts are not configured on this node")
		return
	}
	opts.VolumeMounts = volumeMounts
	ctx, cancel := context.WithTimeout(r.Context(), s.config.RequestTimeout)
	defer cancel()
	created, err := s.runtime.CreateSandbox(ctx, opts)
	if err != nil {
		s.runtimeError(w, err)
		return
	}
	// Build the creation response from its committed result. A zero timeout
	// may already have removed the store record by the time this is written.
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

type sandboxDetail struct {
	sandboxModel
	StartedAt    time.Time          `json:"startedAt"`
	EndAt        time.Time          `json:"endAt"`
	CPUCount     int64              `json:"cpuCount"`
	MemoryMB     int64              `json:"memoryMB"`
	DiskSizeMB   int64              `json:"diskSizeMB"`
	State        string             `json:"state"`
	Metadata     map[string]string  `json:"metadata"`
	VolumeMounts []volumeMountModel `json:"volumeMounts,omitempty"`
}

type volumeMountModel struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (s *Server) model(rec sandbox.Record) sandboxModel {
	result := sandboxModel{TemplateID: rec.SourceTemplateID, SandboxID: rec.ID, ClientID: rec.ID, EnvdVersion: rec.EnvdVersion, Alias: rec.SourceTemplateName}
	if len(s.config.Domains) > 0 {
		domain := s.config.Domains[0]
		result.Domain = &domain
	}
	return result
}

func (s *Server) detail(rec sandbox.Record) sandboxDetail {
	end := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	if rec.ExpiresAt > 0 {
		end = time.Unix(0, rec.ExpiresAt).UTC()
	}
	metadata := rec.Metadata
	if metadata == nil {
		metadata = map[string]string{}
	}
	var mounts []volumeMountModel
	for _, mount := range rec.VolumeMounts {
		mounts = append(mounts, volumeMountModel{Name: mount.Name, Path: mount.Path})
	}
	if mounts == nil {
		mounts = []volumeMountModel{}
	}
	return sandboxDetail{sandboxModel: s.model(rec), StartedAt: time.Unix(0, rec.CreatedAt).UTC(), EndAt: end,
		CPUCount: rec.VCPUNum, MemoryMB: rec.RamMB, State: "running", Metadata: metadata, VolumeMounts: mounts}
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
	if rec.State != sandbox.StateReady {
		writeError(w, 409, "sandbox is not running")
		return
	}
	writeJSON(w, 200, s.detail(rec))
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

// RunExpiry enforces creation timeouts and retries requested runtime cleanup.
// Timeout refresh and automatic pause/resume endpoints remain unimplemented.
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
				// Manager retains UNKNOWN records after an unconfirmed cleanup.
				expired := rec.E2B && rec.ExpiresAt > 0 && rec.ExpiresAt <= now.UnixNano()
				if rec.State != sandbox.StateUnknown && !expired {
					continue
				}
				if err := s.runtime.ReconcileSandbox(ctx, rec.ID, rec.RuntimeID, now); err != nil {
					ulog.Warn("remove expired E2B sandbox", ulog.F("sandbox_id", rec.ID), ulog.F("error", err))
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
