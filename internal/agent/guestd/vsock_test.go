package guestd

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestVsockHandlerRequiresAgentToken(t *testing.T) {
	agentAuth.SetToken("")
	handler := NewVsockHandler("test-version", func() bool { return true })

	resp := handler.HandleMessage("I AM SANDBOX_ID:sandbox-1\n")
	if resp != "NOT_READY\n" {
		t.Fatalf("HandleMessage() = %q, want NOT_READY", resp)
	}
}

func TestVsockHandlerSetsAgentToken(t *testing.T) {
	agentAuth.SetToken("")
	t.Cleanup(func() {
		agentAuth.SetToken("")
	})
	handler := NewVsockHandler("test-version", func() bool { return true })

	resp := handler.HandleMessage("I AM SANDBOX_ID:sandbox-1\nAGENT_TOKEN:secret\n")
	if resp != "OK\nREADY:test-version\n" {
		t.Fatalf("HandleMessage() = %q, want READY response", resp)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(agentTokenMetadataKey, "secret"))
	if err := agentAuth.verify(ctx); err != nil {
		t.Fatalf("agent token was not set from vsock message: %v", err)
	}
}
