package envd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The production endpoints use fixed guest ports. Bind a loopback guest
// address instead of adding alternate ports or transports to production APIs.
const guestIP = "127.0.0.37"

func serveGuest(t *testing.T, port int, handler http.Handler) {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(guestIP, strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
}

func TestNonSecureBootstrap(t *testing.T) {
	var probes atomic.Int32
	initBody := make(chan map[string]any, 1)
	serveGuest(t, DefaultPort, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Access-Token") != "" {
			t.Error("non-secure initialization sent access token")
		}
		switch r.URL.Path {
		case "/health":
			if probes.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		case "/init":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
				t.Error("invalid init HTTP contract")
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			w.Header().Set("X-Envd-Version", "0.8.3")
			initBody <- body
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	client := NewClient()
	if err := client.WaitReady(ctx, guestIP); err != nil {
		t.Fatal(err)
	}
	version, err := client.Init(ctx, guestIP, InitOptions{EnvVars: map[string]string{"MESSAGE": "hello"}, DefaultUser: "user", DefaultWorkdir: "/home/user"})
	if err != nil {
		t.Fatal(err)
	}
	if version != "0.8.3" {
		t.Fatalf("init version = %q", version)
	}
	body := <-initBody
	if _, ok := body["accessToken"]; ok {
		t.Fatal("non-secure init body included accessToken")
	}
	if body["defaultUser"] != "user" || body["defaultWorkdir"] != "/home/user" || body["envVars"].(map[string]any)["MESSAGE"] != "hello" {
		t.Fatalf("init payload = %#v", body)
	}
	if _, err := time.Parse(time.RFC3339Nano, body["timestamp"].(string)); err != nil {
		t.Fatalf("invalid timestamp: %v", err)
	}
	if probes.Load() < 2 {
		t.Fatal("readiness did not retry non-204 health response")
	}
}

func TestHungHealthHonorsBootstrapDeadline(t *testing.T) {
	serveGuest(t, DefaultPort, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := NewClient().WaitReady(ctx, guestIP)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("health deadline: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("hung health request outlived bootstrap deadline")
	}
}

func TestInitRejectsRedirectAndUnexpectedSuccess(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusFound)
	serveGuest(t, DefaultPort, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://example.invalid/steal-init")
		w.WriteHeader(int(status.Load()))
	}))
	client := NewClient()
	for _, code := range []int{http.StatusFound, http.StatusOK, http.StatusInternalServerError} {
		status.Store(int32(code))
		if _, err := client.Init(t.Context(), guestIP, InitOptions{}); err == nil || !strings.Contains(err.Error(), "HTTP "+strconv.Itoa(code)) {
			t.Errorf("init status %d: %v", code, err)
		}
	}
}

// TestInitReportsGuestVersion verifies Init takes the version from the
// X-Envd-Version header envd sets on every /init response, and fails the
// bootstrap when the header is missing or not a valid version.
func TestInitReportsGuestVersion(t *testing.T) {
	for _, test := range []struct {
		name    string
		version string
		ok      bool
	}{
		{"semver", "0.8.3", true},
		{"prerelease", "0.8.3-rc.1+build", true},
		{"missing", "", false},
		{"invalid", "2026.22", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			serveGuest(t, DefaultPort, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.version != "" {
					w.Header().Set("X-Envd-Version", test.version)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			version, err := NewClient().Init(t.Context(), guestIP, InitOptions{})
			if test.ok {
				if err != nil || version != test.version {
					t.Fatalf("version=%q error=%v", version, err)
				}
			} else if err == nil {
				t.Fatalf("accepted invalid version %q", version)
			}
		})
	}
}
