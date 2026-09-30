package netstack

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc/codes"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

func TestValidateL7EgressPolicy(t *testing.T) {
	valid := &SandboxNetworkConfig{
		EgressProxy: &runtimeapi.SandboxEgressProxy{Enabled: true},
		Rules: []runtimeapi.SandboxEgressRule{{
			Action: "allow", Protocol: "https", Host: "*.example.com", Port: 443,
			Method: "GET", PathPrefix: "/api/",
		}},
	}
	if err := validateL7EgressPolicy(valid); err != nil {
		t.Fatalf("validateL7EgressPolicy() error = %v", err)
	}

	tests := []struct {
		name string
		cfg  *SandboxNetworkConfig
	}{
		{
			name: "proxy disabled",
			cfg:  &SandboxNetworkConfig{Rules: []runtimeapi.SandboxEgressRule{{Action: "allow", Host: "example.com"}}},
		},
		{
			name: "invalid action",
			cfg: &SandboxNetworkConfig{EgressProxy: &runtimeapi.SandboxEgressProxy{Enabled: true}, Rules: []runtimeapi.SandboxEgressRule{{
				Action: "audit", Host: "example.com",
			}}},
		},
		{
			name: "invalid protocol",
			cfg: &SandboxNetworkConfig{EgressProxy: &runtimeapi.SandboxEgressProxy{Enabled: true}, Rules: []runtimeapi.SandboxEgressRule{{
				Action: "allow", Protocol: "ssh", Host: "example.com",
			}}},
		},
		{
			name: "invalid host",
			cfg: &SandboxNetworkConfig{EgressProxy: &runtimeapi.SandboxEgressProxy{Enabled: true}, Rules: []runtimeapi.SandboxEgressRule{{
				Action: "allow", Host: "localhost",
			}}},
		},
		{
			name: "invalid path prefix",
			cfg: &SandboxNetworkConfig{EgressProxy: &runtimeapi.SandboxEgressProxy{Enabled: true}, Rules: []runtimeapi.SandboxEgressRule{{
				Action: "allow", Host: "example.com", PathPrefix: "api",
			}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateL7EgressPolicy(tt.cfg); err == nil {
				t.Fatal("validateL7EgressPolicy() accepted invalid policy")
			}
		})
	}
}

func TestMatchingEgressRuleDenyTakesPrecedence(t *testing.T) {
	rules := []runtimeapi.SandboxEgressRule{
		{Action: "allow", Protocol: "https", Host: "*.example.com", Method: "GET", PathPrefix: "/api/"},
		{Action: "deny", Protocol: "https", Host: "blocked.example.com"},
	}

	rule, ok := matchingEgressRule(rules, &authv3.AttributeContext_HttpRequest{
		Scheme: "https", Host: "blocked.example.com", Method: "GET", Path: "/api/items",
	})
	if !ok || rule.Action != "deny" {
		t.Fatalf("matchingEgressRule() = (%+v, %v), want deny", rule, ok)
	}

	_, ok = matchingEgressRule(rules, &authv3.AttributeContext_HttpRequest{
		Scheme: "https", Host: "example.com", Method: "GET", Path: "/api/items",
	})
	if ok {
		t.Fatal("wildcard rule matched the apex domain")
	}
}

func TestEgressAuthorizationIsIsolatedBySandboxSource(t *testing.T) {
	controller := &egressSecurityController{policies: map[string]compiledEgressPolicy{
		"10.12.0.2": {
			sandboxID: "sandbox-allow",
			rules:     []runtimeapi.SandboxEgressRule{{Action: "allow", Protocol: "https", Host: "allowed.example.com"}},
		},
		"10.12.0.3": {
			sandboxID: "sandbox-deny",
			rules:     []runtimeapi.SandboxEgressRule{{Action: "deny", Protocol: "https", Host: "allowed.example.com"}},
		},
	}}

	allowed, err := controller.Check(t.Context(), egressCheckRequest("10.12.0.2", "allowed.example.com"))
	if err != nil {
		t.Fatalf("Check(allowed) error = %v", err)
	}
	if allowed.GetStatus().GetCode() != int32(codes.OK) {
		t.Fatalf("Check(allowed) status = %v, want OK", allowed.GetStatus())
	}
	if headers := allowed.GetOkResponse().GetHeaders(); len(headers) != 0 {
		t.Fatalf("Check(allowed) mutated request headers: %v", headers)
	}

	for _, sourceIP := range []string{"10.12.0.3", "10.12.0.4"} {
		denied, err := controller.Check(t.Context(), egressCheckRequest(sourceIP, "allowed.example.com"))
		if err != nil {
			t.Fatalf("Check(%s) error = %v", sourceIP, err)
		}
		if denied.GetStatus().GetCode() != int32(codes.PermissionDenied) {
			t.Fatalf("Check(%s) status = %v, want PermissionDenied", sourceIP, denied.GetStatus())
		}
	}
}

func egressCheckRequest(sourceIP, host string) *authv3.CheckRequest {
	return &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Source: &authv3.AttributeContext_Peer{Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{Address: sourceIP}},
		}},
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
			Scheme: "https", Host: host, Method: "GET", Path: "/",
		}},
	}}
}
