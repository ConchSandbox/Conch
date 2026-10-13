package netstack

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

func TestValidateL7EgressPolicy(t *testing.T) {
	valid := &SandboxNetworkConfig{
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
			name: "invalid default action",
			cfg:  &SandboxNetworkConfig{DefaultAction: "audit"},
		},
		{
			name: "invalid action",
			cfg: &SandboxNetworkConfig{Rules: []runtimeapi.SandboxEgressRule{{
				Action: "audit", Host: "example.com",
			}}},
		},
		{
			name: "invalid protocol",
			cfg: &SandboxNetworkConfig{Rules: []runtimeapi.SandboxEgressRule{{
				Action: "allow", Protocol: "ssh", Host: "example.com",
			}}},
		},
		{
			name: "invalid host",
			cfg: &SandboxNetworkConfig{Rules: []runtimeapi.SandboxEgressRule{{
				Action: "allow", Host: "localhost",
			}}},
		},
		{
			name: "invalid path prefix",
			cfg: &SandboxNetworkConfig{Rules: []runtimeapi.SandboxEgressRule{{
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
		{Action: " deny ", Protocol: " https ", Host: "blocked.example.com"},
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

func TestEgressAuthZIsolatedBySandboxSource(t *testing.T) {
	controller := &egressSecurityController{policies: map[string]compiledEgressPolicy{
		"10.12.0.2": {
			sandboxID: "sandbox-allow",
			rules:     []runtimeapi.SandboxEgressRule{{Action: "allow", Protocol: "https", Host: "allowed.example.com"}},
		},
		"10.12.0.3": {
			sandboxID: "sandbox-deny",
			rules:     []runtimeapi.SandboxEgressRule{{Action: " deny ", Protocol: "https", Host: "allowed.example.com"}},
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

func TestEgressAuthZDefaultAction(t *testing.T) {
	for _, action := range []string{"", "deny", "allow"} {
		t.Run("default_"+action, func(t *testing.T) {
			controller := &egressSecurityController{policies: map[string]compiledEgressPolicy{
				"10.12.0.2": {
					sandboxID: "sandbox", defaultAction: action,
					rules: []runtimeapi.SandboxEgressRule{
						{Action: "allow", Host: "allowed.example.com"},
						{Action: "deny", Host: "blocked.example.com"},
					},
				},
			}}
			for _, host := range []string{"allowed.example.com", "blocked.example.com", "other.example.com"} {
				response, err := controller.Check(t.Context(), egressCheckRequest("10.12.0.2", host))
				if err != nil {
					t.Fatal(err)
				}
				want := codes.PermissionDenied
				if host == "allowed.example.com" || (host == "other.example.com" && action == "allow") {
					want = codes.OK
				}
				if response.GetStatus().GetCode() != int32(want) {
					t.Fatalf("Check(%s) = %v, want %v", host, response.GetStatus(), want)
				}
			}
		})
	}
}

func TestEgressAuthZCombinesIPAndDomain(t *testing.T) {
	for _, tt := range []struct {
		name   string
		policy compiledEgressPolicy
		want   codes.Code
	}{
		{"IP allow", compiledEgressPolicy{allowOut: []string{"198.51.100.0/24"}}, codes.OK},
		{"domain allow outside IP allowlist", compiledEgressPolicy{
			allowOut: []string{"192.0.2.1"},
			rules:    []runtimeapi.SandboxEgressRule{{Action: "allow", Host: "example.com"}},
		}, codes.OK},
		{"IP deny overrides domain allow", compiledEgressPolicy{
			denyOut: []string{"198.51.100.0/24"},
			rules:   []runtimeapi.SandboxEgressRule{{Action: "allow", Host: "example.com"}},
		}, codes.PermissionDenied},
		{"domain deny overrides IP allow", compiledEgressPolicy{
			allowOut: []string{"198.51.100.1"},
			rules:    []runtimeapi.SandboxEgressRule{{Action: "deny", Host: "example.com"}},
		}, codes.PermissionDenied},
		{"IP deny overrides default allow", compiledEgressPolicy{
			defaultAction: "allow", denyOut: []string{"198.51.100.1"},
		}, codes.PermissionDenied},
	} {
		t.Run(tt.name, func(t *testing.T) {
			controller := &egressSecurityController{policies: map[string]compiledEgressPolicy{"10.12.0.2": tt.policy}}
			response, err := controller.Check(t.Context(), egressCheckRequest("10.12.0.2", "example.com"))
			if err != nil || response.GetStatus().GetCode() != int32(tt.want) {
				t.Fatalf("Check() = %v, %v; want %v", response, err, tt.want)
			}
		})
	}
}

func TestEgressAuthZRejectsMissingUpstreamAddress(t *testing.T) {
	controller := &egressSecurityController{policies: map[string]compiledEgressPolicy{
		"10.12.0.2": {defaultAction: "allow"},
	}}
	request := egressCheckRequest("10.12.0.2", "example.com")
	request.Attributes.MetadataContext = nil
	response, err := controller.Check(t.Context(), request)
	if err != nil || response.GetStatus().GetCode() != int32(codes.PermissionDenied) {
		t.Fatalf("Check() = %v, %v; want PermissionDenied", response, err)
	}
}

func egressCheckRequest(sourceIP, host string) *authv3.CheckRequest {
	return &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		MetadataContext: &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{
			"conch.egress": {Fields: map[string]*structpb.Value{
				"upstream_address": structpb.NewStringValue("198.51.100.1:443"),
			}},
		}},
		Source: &authv3.AttributeContext_Peer{Address: &corev3.Address{
			Address: &corev3.Address_SocketAddress{SocketAddress: &corev3.SocketAddress{Address: sourceIP}},
		}},
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
			Scheme: "https", Host: host, Method: "GET", Path: "/",
		}},
	}}
}
