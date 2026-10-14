// Package envd bootstraps the non-secure E2B daemon after conch-init has
// completed guest network initialization. It does not replace conch-init.
package envd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const DefaultPort = 49983

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// InitOptions is the non-secure subset of envd's POST /init contract. Access
// tokens and MMDS initialization are deliberately unsupported in this phase.
type InitOptions struct {
	EnvVars        map[string]string `json:"envVars,omitempty"`
	DefaultWorkdir string            `json:"defaultWorkdir,omitempty"`
	DefaultUser    string            `json:"defaultUser,omitempty"`
}

type Client struct {
	http *http.Client
}

func NewClient() *Client {
	return &Client{http: &http.Client{
		Transport: &http.Transport{
			// Interaction IPs are reused. A previous guest's idle connection
			// must never be used to initialize its successor.
			DisableKeepAlives: true,
			DialContext:       (&net.Dialer{Timeout: time.Second}).DialContext,
		},
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func endpoint(ip, path string) (string, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil || addr.Zone() != "" || addr.IsUnspecified() || addr.IsMulticast() {
		return "", fmt.Errorf("invalid envd interaction IP %q", ip)
	}
	return "http://" + net.JoinHostPort(addr.String(), strconv.Itoa(DefaultPort)) + path, nil
}

// WaitReady polls envd's GET /health until it returns 204. The caller's context
// bounds the entire bootstrap; each individual probe is also bounded. The real
// envd health endpoint provides no version information.
func (c *Client) WaitReady(ctx context.Context, ip string) error {
	url, err := endpoint(ip, "/health")
	if err != nil {
		return err
	}
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for envd health (last probe: %v): %w", lastErr, err)
		}
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
		if err == nil {
			var resp *http.Response
			resp, err = c.http.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode != http.StatusNoContent {
					err = fmt.Errorf("GET /health returned HTTP %d", resp.StatusCode)
				}
			}
		}
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

// Init sets envd execution defaults and synchronizes guest time using the
// upstream /init schema, and returns the installed envd version. A successful
// HTTP 204 is required before publishing a sandbox's proxy route. The version
// comes from the X-Envd-Version header envd sets on every /init response
// before any parsing or auth — the same signal the upstream orchestrator reads
// off the /init it already makes — so no separate probe is needed. This keeps
// the E2B response tied to the actual guest rather than a configured guess.
// The payload never includes an access token.
func (c *Client) Init(ctx context.Context, ip string, options InitOptions) (string, error) {
	url, err := endpoint(ip, "/init")
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		InitOptions
		Timestamp time.Time `json:"timestamp"`
	}{options, time.Now().UTC()})
	if err != nil {
		return "", fmt.Errorf("encode envd init: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("initialize envd: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		// Bound diagnostics: an unhealthy guest must not make the Node buffer
		// an arbitrary response while reporting bootstrap failure.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("initialize envd: HTTP %d: %s", resp.StatusCode, body)
	}
	version := strings.TrimSpace(resp.Header.Get("X-Envd-Version"))
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("envd init response lacks valid X-Envd-Version %q", version)
	}
	return version, nil
}
