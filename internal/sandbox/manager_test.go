package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestWaitForVsockAgentReadyReportsTimeout(t *testing.T) {
	readyCh := make(chan error, 1)

	go waitForVsockAgentReady(
		context.Background(),
		nil,
		"sandbox-timeout",
		t.TempDir()+"/missing.vsock",
		time.Millisecond,
		10*time.Millisecond,
		readyCh,
	)

	select {
	case err := <-readyCh:
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
