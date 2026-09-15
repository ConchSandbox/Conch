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

	"connectrpc.com/connect"
	pb "github.com/openeuler/Conch/api/go_proto"
	"github.com/openeuler/Conch/api/go_proto/pbconnect"
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
	if err := client.Init(ctx, guestIP, InitOptions{EnvVars: map[string]string{"MESSAGE": "hello"}, DefaultUser: "user", DefaultWorkdir: "/home/user"}); err != nil {
		t.Fatal(err)
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
		if err := client.Init(t.Context(), guestIP, InitOptions{}); err == nil || !strings.Contains(err.Error(), "HTTP "+strconv.Itoa(code)) {
			t.Errorf("init status %d: %v", code, err)
		}
	}
}

type versionProcess struct {
	pbconnect.UnimplementedProcessServiceHandler
	t       *testing.T
	version string
	exit    int32
}

func (s versionProcess) StartProcess(_ context.Context, request *connect.Request[pb.StartProcessRequest], stream *connect.ServerStream[pb.ProcessEvent]) error {
	if request.Msg.Cmd != "/usr/bin/envd" || len(request.Msg.Args) != 1 || request.Msg.Args[0] != "-version" || request.Header().Get("conch-init-token") != "secret" {
		s.t.Errorf("invalid version probe: %#v", request.Msg)
	}
	if err := stream.Send(&pb.ProcessEvent{Event: &pb.ProcessEvent_Data{Data: &pb.ProcessDataEvent{Output: &pb.ProcessDataEvent_Stdout{Stdout: []byte(s.version + "\n")}}}}); err != nil {
		return err
	}
	return stream.Send(&pb.ProcessEvent{Event: &pb.ProcessEvent_End{End: &pb.ProcessEndEvent{Exited: true, ExitCode: s.exit}}})
}

func TestVersionReadsActualGuestCommand(t *testing.T) {
	for _, test := range []struct {
		version string
		exit    int32
		ok      bool
	}{
		{"0.8.3", 0, true},
		{"0.8.3-rc.1+build", 0, true},
		{"2026.22", 0, false},
		{"0.8.3", 1, false},
	} {
		t.Run(test.version+strconv.Itoa(int(test.exit)), func(t *testing.T) {
			_, handler := pbconnect.NewProcessServiceHandler(versionProcess{t: t, version: test.version, exit: test.exit})
			serveGuest(t, 4064, handler)
			version, err := NewClient().Version(t.Context(), guestIP, "secret")
			if test.ok {
				if err != nil || version != test.version {
					t.Fatalf("version=%q error=%v", version, err)
				}
			} else if err == nil {
				t.Fatalf("accepted invalid version probe %q", version)
			}
		})
	}
}
