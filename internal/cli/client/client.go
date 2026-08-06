package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	conchconfig "github.com/openeuler/Conch/internal/config"
)

const (
	defaultConchAPIURL = "http://localhost:4063"
	defaultUnixAPIURL  = "http://conchd-unix"
	defaultHTTPTimeout = 120 * time.Second
)

// Options controls conchd API endpoint discovery and request timeout.
type Options struct {
	BaseURL    string
	ConfigPath string
	Timeout    time.Duration
}

// Client communicates with the conchd HTTP API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New creates a conchd API client.
func New(opts Options) (*Client, error) {
	baseURL, httpClient, err := resolveTransport(opts)
	if err != nil {
		return nil, err
	}
	return &Client{baseURL: baseURL, httpClient: httpClient}, nil
}

func resolveTransport(opts Options) (string, *http.Client, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = resolveHTTPTimeout()
	}
	configPath := opts.ConfigPath
	if configPath == "" {
		configPath = conchconfig.FindConfigFile()
	}
	cfg, err := conchconfig.LoadConfig(configPath)
	if err != nil {
		return "", nil, fmt.Errorf("load conch client config: %w", err)
	}
	if baseURL := normalizeBaseURL(opts.BaseURL); baseURL != "" {
		return baseURL, &http.Client{Timeout: timeout}, nil
	}
	if baseURL := normalizeBaseURL(os.Getenv("CONCH_API_URL")); baseURL != "" {
		return baseURL, &http.Client{Timeout: timeout}, nil
	}
	if unixSocket := strings.TrimSpace(cfg.GetServerUnixSocket()); unixSocket != "" {
		return defaultUnixAPIURL, newUnixSocketHTTPClient(unixSocket, timeout), nil
	}
	host := strings.TrimSpace(cfg.Server.Host)
	if host != "" && cfg.Server.Port > 0 {
		return fmt.Sprintf("http://%s:%d", host, cfg.Server.Port), &http.Client{Timeout: timeout}, nil
	}

	host = strings.TrimSpace(os.Getenv("CONCHD_HOST"))
	port := strings.TrimSpace(os.Getenv("CONCHD_PORT"))
	if host != "" {
		if port == "" {
			port = "4063"
		}
		return fmt.Sprintf("http://%s:%s", host, port), &http.Client{Timeout: timeout}, nil
	}
	return defaultConchAPIURL, &http.Client{Timeout: timeout}, nil
}

func normalizeBaseURL(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

func resolveHTTPTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CONCH_API_TIMEOUT"))
	if raw == "" {
		return defaultHTTPTimeout
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout <= 0 {
		return defaultHTTPTimeout
	}
	return timeout
}

func newUnixSocketHTTPClient(socketPath string, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

func (c *Client) postJSON(ctx context.Context, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	return decodeResponse(resp, path, out)
}

func decodeResponse(resp *http.Response, path string, out any) error {
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("%s returned status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}
