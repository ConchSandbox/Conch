package hostconn

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestWaitForVsockAgentReadyReportsTimeout(t *testing.T) {
	errCh := make(chan error, 1)
	go func() {
		conn, err := WaitReady(
			context.Background(),
			"sandbox-timeout",
			"token",
			t.TempDir()+"/missing.vsock",
			time.Millisecond,
			10*time.Millisecond,
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
		"sandbox-canceled",
		"token",
		t.TempDir()+"/missing.vsock",
		time.Millisecond,
		time.Second,
	)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil || err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
