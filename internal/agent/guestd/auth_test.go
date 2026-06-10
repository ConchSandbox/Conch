package guestd

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
)

func TestAgentAuthVerify(t *testing.T) {
	agentAuth.SetToken("secret")
	t.Cleanup(func() {
		agentAuth.SetToken("")
	})

	if err := agentAuth.verify(context.Background()); err == nil {
		t.Fatal("verify() error = nil, want missing token error")
	}
	wrongCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(agentTokenMetadataKey, "wrong"))
	if err := agentAuth.verify(wrongCtx); err == nil {
		t.Fatal("verify() error = nil, want invalid token error")
	}
	okCtx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(agentTokenMetadataKey, "secret"))
	if err := agentAuth.verify(okCtx); err != nil {
		t.Fatalf("verify() error = %v, want nil", err)
	}
}

func TestAgentAuthRequiresInitializedToken(t *testing.T) {
	agentAuth.SetToken("")
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(agentTokenMetadataKey, "secret"))
	if err := agentAuth.verify(ctx); err == nil {
		t.Fatal("verify() error = nil, want uninitialized token error")
	}
}
