package guestd

import (
	"context"
	"crypto/subtle"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const agentTokenMetadataKey = "x-conch-agent-token"

var agentAuth = &agentTokenAuth{}

type agentTokenAuth struct {
	mu    sync.RWMutex
	token string
}

func (a *agentTokenAuth) SetToken(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = strings.TrimSpace(token)
}

func (a *agentTokenAuth) verify(ctx context.Context) error {
	a.mu.RLock()
	expected := a.token
	a.mu.RUnlock()
	if expected == "" {
		return status.Error(codes.Unauthenticated, "agent token is not initialized")
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "agent token is required")
	}
	values := md.Get(agentTokenMetadataKey)
	if len(values) == 0 || values[0] == "" {
		return status.Error(codes.Unauthenticated, "agent token is required")
	}
	got := values[0]
	if len(got) > 4096 {
		return status.Error(codes.Unauthenticated, "agent token is invalid")
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		return status.Error(codes.Unauthenticated, "agent token is invalid")
	}
	return nil
}

func agentUnaryAuthInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := agentAuth.verify(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func agentStreamAuthInterceptor(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := agentAuth.verify(stream.Context()); err != nil {
		return err
	}
	return handler(srv, stream)
}
