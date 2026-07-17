package hostconn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/pkg/ulog"
	"golang.org/x/sys/unix"
)

func TestWaitForVsockAgentReadyReportsTimeout(t *testing.T) {
	errCh := make(chan error, 1)
	go func() {
		conn, err := WaitReady(
			context.Background(),
			ReadyOptions{
				SandboxID:       "sandbox-timeout",
				AgentToken:      "token",
				VsockSocketPath: t.TempDir() + "/missing.vsock",
				Retry:           time.Millisecond,
				Timeout:         10 * time.Millisecond,
			},
		)
		if conn != nil {
			_ = conn.Close()
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected timeout error")
		}
		if !strings.Contains(err.Error(), "vsock signal attempts timed out") {
			t.Fatalf("error = %q, want timeout", err.Error())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ready result")
	}
}

func TestWaitReadyReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	conn, err := WaitReady(
		ctx,
		ReadyOptions{
			SandboxID:       "sandbox-canceled",
			AgentToken:      "token",
			VsockSocketPath: t.TempDir() + "/missing.vsock",
			Retry:           time.Millisecond,
			Timeout:         time.Second,
		},
	)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil || err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestReadyPayloadIncludesEnvironment(t *testing.T) {
	payload, err := readyPayload(ReadyOptions{
		SandboxID:  "sandbox-1",
		AgentToken: "token",
		Env: map[string]string{
			"SOME_RANDOM_KEY": "key123",
		},
	})
	if err != nil {
		t.Fatalf("readyPayload() error = %v", err)
	}
	want := "I AM SANDBOX_ID:sandbox-1\nAGENT_TOKEN:token\nENV_JSON:{\"SOME_RANDOM_KEY\":\"key123\"}\n"
	if payload != want {
		t.Fatalf("readyPayload() = %q, want %q", payload, want)
	}
}

func TestValidateAgentReadyAcceptsVersionMismatch(t *testing.T) {
	if err := validateAgentReady("READY:0.0.0", "sandbox-version", ulog.GetLogger(), ""); err != nil {
		t.Fatalf("validateAgentReady() error = %v, want nil for version mismatch", err)
	}
}

func TestValidateAgentReadyRejectsNotReadyAndUnknown(t *testing.T) {
	if err := validateAgentReady("NOT_READY", "sandbox-not-ready", ulog.GetLogger(), ""); err == nil {
		t.Fatal("validateAgentReady(NOT_READY) error = nil, want error")
	}
	if err := validateAgentReady("hello", "sandbox-unknown", ulog.GetLogger(), ""); err == nil {
		t.Fatal("validateAgentReady(unknown) error = nil, want error")
	}
}

func TestIsVsockUnsupported(t *testing.T) {
	if !isVsockUnsupported(unix.EAFNOSUPPORT) {
		t.Fatal("EAFNOSUPPORT should be unsupported")
	}
	if isVsockUnsupported(unix.ENODEV) {
		t.Fatal("ENODEV should stay retryable")
	}
	wrapped := fmt.Errorf("%w: %w", errVsockUnsupported, unix.EAFNOSUPPORT)
	if !errors.Is(wrapped, errVsockUnsupported) {
		t.Fatal("wrapped errVsockUnsupported should match errors.Is")
	}
}
