package guestd

import (
	"net/http"
	"testing"
)

func TestAgentAuthVerifyHTTPHeader(t *testing.T) {
	agentAuth.SetToken("secret")
	t.Cleanup(func() {
		agentAuth.SetToken("")
	})

	if err := agentAuth.verifyHTTPHeader(http.Header{}); err == nil {
		t.Fatal("verifyHTTPHeader() error = nil, want missing token error")
	}
	wrongHeader := http.Header{}
	wrongHeader.Set(agentTokenHeaderKey, "wrong")
	if err := agentAuth.verifyHTTPHeader(wrongHeader); err == nil {
		t.Fatal("verifyHTTPHeader() error = nil, want invalid token error")
	}
	okHeader := http.Header{}
	okHeader.Set(agentTokenHeaderKey, "secret")
	if err := agentAuth.verifyHTTPHeader(okHeader); err != nil {
		t.Fatalf("verifyHTTPHeader() error = %v, want nil", err)
	}
}

func TestAgentAuthRequiresInitializedToken(t *testing.T) {
	agentAuth.SetToken("")
	header := http.Header{}
	header.Set(agentTokenHeaderKey, "secret")
	if err := agentAuth.verifyHTTPHeader(header); err == nil {
		t.Fatal("verifyHTTPHeader() error = nil, want uninitialized token error")
	}
}
