package sandboxproxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	SandboxIDHeader     = "X-Agentenv-Sandbox-Id"
	TargetPortHeader    = "X-Agentenv-Target-Port"
	E2BSandboxIDHeader  = "E2b-Sandbox-Id"
	E2BTargetPortHeader = "E2b-Sandbox-Port"
)

type Handler struct {
	registry  *Registry
	domains   []string
	transport *http.Transport
}

func NewHandler(registry *Registry, domains []string) *Handler {
	normalized := make([]string, 0, len(domains))
	for _, domain := range domains {
		if domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), "."); domain != "" {
			normalized = append(normalized, domain)
		}
	}
	return &Handler{
		registry: registry,
		domains:  normalized,
		transport: &http.Transport{
			// No environment proxy, DNS, or cross-generation idle pool: the
			// registry's interaction IP is the sole source of upstream hosts.
			DisableKeepAlives:     true,
			DisableCompression:    true,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
}

func hasPrefix(path string) bool {
	return path == "/proxy" || strings.HasPrefix(path, "/proxy/")
}

// IsHostRoute must be checked before control-plane routes so an application
// path such as /sandboxes is still forwarded when its Host identifies a guest.
// Malformed matching host routes are included and receive a proxy error.
func (h *Handler) IsHostRoute(r *http.Request) bool {
	_, matched, _ := h.hostRoute(r.Host)
	return !hasPrefix(r.URL.Path) && matched
}

// IsProxyRequest classifies explicit and fallback routing. Header-based
// fallback is checked after concrete control-plane routes, as in AgentENV.
func (h *Handler) IsProxyRequest(r *http.Request) bool {
	return hasPrefix(r.URL.Path) || h.IsHostRoute(r) || hasHeader(r.Header, SandboxIDHeader) || hasHeader(r.Header, E2BSandboxIDHeader)
}

type destination struct {
	id   string
	port uint16
}

func (h *Handler) hostRoute(host string) (destination, bool, error) {
	if hostname, _, err := net.SplitHostPort(host); err == nil {
		host = hostname
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	for _, domain := range h.domains {
		label, ok := strings.CutSuffix(host, "."+domain)
		if !ok || label == "" || strings.Contains(label, ".") {
			continue
		}
		port, id, ok := strings.Cut(label, "-")
		if !ok {
			continue
		}
		result, err := parseDestination(id, port)
		return result, true, err
	}
	return destination{}, false, nil
}

func hasHeader(header http.Header, name string) bool {
	_, ok := header[http.CanonicalHeaderKey(name)]
	return ok
}

func firstHeader(header http.Header, primary, alias string) string {
	if hasHeader(header, primary) {
		return header.Get(primary)
	}
	return header.Get(alias)
}

func parseDestination(id, port string) (destination, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return destination{}, errors.New("missing or invalid sandbox routing header")
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return destination{}, errors.New("missing or invalid target port routing header")
	}
	return destination{id: parsed.String(), port: uint16(p)}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.registry.isClosed() {
		writeError(w, http.StatusServiceUnavailable, "sandbox proxy is shutting down")
		return
	}
	var target destination
	var err error
	matched := false
	if !hasPrefix(r.URL.Path) {
		target, matched, err = h.hostRoute(r.Host)
	}
	if !matched {
		target, err = parseDestination(firstHeader(r.Header, SandboxIDHeader, E2BSandboxIDHeader), firstHeader(r.Header, TargetPortHeader, E2BTargetPortHeader))
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	route, ok := h.registry.Lookup(target.id)
	if !ok {
		if _, exists := h.registry.CurrentGeneration(target.id); exists {
			writeError(w, http.StatusBadGateway, "sandbox route is temporarily unavailable")
		} else {
			writeError(w, http.StatusNotFound, "sandbox not found")
		}
		return
	}
	ctx, cancel := route.requestContext(r.Context())
	defer cancel()
	if route.lifetime.Err() != nil {
		writeError(w, http.StatusGone, "sandbox runtime is no longer active")
		return
	}
	upstream := *r.URL
	upstream.Scheme = "http"
	upstream.Host = net.JoinHostPort(route.IP, strconv.Itoa(int(target.port)))
	upstream.User = nil
	if hasPrefix(upstream.Path) {
		stripProxyPrefix(&upstream)
	}
	// Preserve simultaneous upload and response streaming over HTTP/1, in
	// addition to SSE and Connect server streams. WebSocket is bridged by the
	// standard reverse proxy, including cancellation on runtime invalidation.
	_ = http.NewResponseController(w).EnableFullDuplex()
	proxy := &httputil.ReverseProxy{
		Transport:     h.transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			*pr.Out.URL = upstream
			pr.Out.Host = upstream.Host
			if mask := route.MaskRequestHost; mask != "" {
				// E2B's maskRequestHost hides the sandbox domain from the
				// guest: the masked value replaces the Host header, and the
				// original host stays recoverable via X-Forwarded-Host.
				masked := strings.ReplaceAll(mask, "${PORT}", strconv.Itoa(int(target.port)))
				if existing := pr.Out.Header.Get("X-Forwarded-Host"); existing == "" {
					pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
				} else {
					pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host+", "+existing)
				}
				pr.Out.Host = masked
			}
			for _, header := range []string{SandboxIDHeader, E2BSandboxIDHeader, TargetPortHeader, E2BTargetPortHeader, "X-API-Key", "X-Access-Token", "E2b-Traffic-Access-Token", "Conch-Init-Token"} {
				pr.Out.Header.Del(header)
			}
			// httputil removes hop-by-hop and untrusted forwarded headers.
			// Application Authorization, Connect headers and WS subprotocols
			// remain application-owned and pass through unchanged.
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Method", pr.In.Method)
			pr.Out.Header.Set("X-Forwarded-Uri", upstream.RequestURI())
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				writeError(w, http.StatusGone, "sandbox request was canceled")
			} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				writeError(w, http.StatusGatewayTimeout, "sandbox upstream timed out")
			} else {
				writeError(w, http.StatusBadGateway, "sandbox upstream unavailable")
			}
		},
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

// requestContext keeps generation invalidation on the synchronous parent
// cancellation path. Resource release must not race a scheduled AfterFunc
// before a proxy request observes that its old guest IP is no longer valid.
func (route Route) requestContext(request context.Context) (context.Context, context.CancelFunc) {
	var ctx context.Context
	var cancel context.CancelFunc
	if deadline, ok := request.Deadline(); ok {
		ctx, cancel = context.WithDeadline(route.lifetime, deadline)
	} else {
		ctx, cancel = context.WithCancel(route.lifetime)
	}
	stop := context.AfterFunc(request, cancel)
	if request.Err() != nil {
		cancel()
	}
	return ctx, func() {
		stop()
		cancel()
	}
}

func stripProxyPrefix(u *url.URL) {
	if raw := u.RawPath; raw != "" {
		// RawPath can encode characters inside /proxy. Find the escaped
		// boundary without losing escaped slashes elsewhere in the path.
		for cut := 1; cut <= len(raw); cut++ {
			decoded, err := url.PathUnescape(raw[:cut])
			if err == nil && decoded == "/proxy" {
				u.RawPath = raw[cut:]
				break
			}
		}
	}
	u.Path = strings.TrimPrefix(u.Path, "/proxy")
	if u.Path == "" {
		u.Path = "/"
		u.RawPath = ""
	}
}

func writeError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{code, message})
}
