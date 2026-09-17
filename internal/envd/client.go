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

	"connectrpc.com/connect"
	pb "github.com/openeuler/Conch/api/go_proto"
	"github.com/openeuler/Conch/api/go_proto/pbconnect"
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
// upstream /init schema. A successful HTTP 204 is required before publishing a
// sandbox's proxy route. The payload never includes an access token.
func (c *Client) Init(ctx context.Context, ip string, options InitOptions) error {
	url, err := endpoint(ip, "/init")
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		InitOptions
		Timestamp time.Time `json:"timestamp"`
	}{options, time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("encode envd init: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("initialize envd: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		// Bound diagnostics: an unhealthy guest must not make the Node buffer
		// an arbitrary response while reporting bootstrap failure.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("initialize envd: HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}

// Version reads the installed binary's version through conch-init's existing
// authenticated Connect API. envd /health itself has no version field. This
// keeps the E2B response tied to the actual guest rather than a configured guess.
func (c *Client) Version(ctx context.Context, ip, conchInitToken string) (string, error) {
	base, err := endpoint(ip, "")
	if err != nil {
		return "", err
	}
	if conchInitToken == "" {
		return "", fmt.Errorf("conch-init token is required for envd version discovery")
	}
	// conch-init accepts Connect server streams over both HTTP/1 and h2c.
	base = strings.TrimSuffix(base, ":"+strconv.Itoa(DefaultPort)) + ":4064"
	client := pbconnect.NewProcessServiceClient(c.http, base)
	req := connect.NewRequest(&pb.StartProcessRequest{Cmd: "/usr/bin/envd", Args: []string{"-version"}, Cwd: "/"})
	req.Header().Set("conch-init-token", conchInitToken)
	// guestd bounds child lifetime using this standard Connect header.
	req.Header().Set("Connect-Timeout-Ms", "10000")
	stream, err := client.StartProcess(ctx, req)
	if err != nil {
		return "", fmt.Errorf("read envd version: %w", err)
	}
	defer stream.Close()
	var output strings.Builder
	var end *pb.ProcessEndEvent
	for stream.Receive() {
		message := stream.Msg()
		if data := message.GetData(); data != nil {
			if output.Len()+len(data.GetStdout()) > 4096 {
				return "", fmt.Errorf("envd version output exceeds 4096 bytes")
			}
			output.Write(data.GetStdout())
		}
		if message.GetEnd() != nil {
			end = message.GetEnd()
		}
	}
	if err := stream.Err(); err != nil {
		return "", fmt.Errorf("read envd version: %w", err)
	}
	if end == nil || !end.Exited || end.ExitCode != 0 || end.Error != "" {
		return "", fmt.Errorf("envd version command did not exit successfully")
	}
	version := strings.TrimSpace(output.String())
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("guest returned invalid envd version %q", version)
	}
	return version, nil
}
