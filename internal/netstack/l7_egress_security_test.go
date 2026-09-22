package netstack

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
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
			Method: "GET", Path: "/api/**",
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
			name: "invalid path",
			cfg: &SandboxNetworkConfig{Rules: []runtimeapi.SandboxEgressRule{{
				Action: "allow", Host: "example.com", Path: "api",
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

func TestEgressPathPatterns(t *testing.T) {
	for _, pattern := range []string{"", "/", "/api/user", "/api/*/read", "/api/**"} {
		if !validEgressPath(pattern) {
			t.Errorf("validEgressPath(%q) = false", pattern)
		}
	}
	for _, pattern := range []string{"api", "/api/u*", "/api/**/read", "/api/***", "/api/?x=1", "/api/#part", "/api/\\read"} {
		if err := validateL7EgressPolicy(&SandboxNetworkConfig{Rules: []runtimeapi.SandboxEgressRule{{
			Action: "allow", Host: "example.com", Path: pattern,
		}}}); err == nil {
			t.Errorf("validation accepted path %q", pattern)
		}
	}
	for _, tt := range []struct {
		pattern string
		path    string
		want    bool
	}{
		{"", "/anything", true},
		{"/", "/", true},
		{"/api/user", "/api/user?next=/write", true},
		{"/api/user", "/api/user/read", false},
		{"/api/user", "/api/user/", false},
		{"/api/user", "/API/user", false},
		{"/api/user", "/api/%75ser", false},
		{"/api/user", "/api/other/../user", false},
		{"/api/*/read", "/api/user/read", true},
		{"/api/*/read", "/api/user/read?x=1", true},
		{"/api/*/read", "/api//read", false},
		{"/api/*/read", "/api/team/user/read", false},
		{"/api/*/read", "/api/user/write", false},
		{"/api/**", "/api/", true},
		{"/api/**", "/api/team/user/read", true},
		{"/api/**", "/api", false},
		{"/api/**", "/apix/user", false},
	} {
		if got := egressPathMatches(tt.pattern, tt.path); got != tt.want {
			t.Errorf("egressPathMatches(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestMatchingEgressRulePathDenyTakesPrecedence(t *testing.T) {
	rules := []runtimeapi.SandboxEgressRule{
		{Action: "allow", Host: "example.com", Path: "/api/**"},
		{Action: "deny", Host: "example.com", Path: "/api/*/write"},
	}
	for _, tt := range []struct {
		path string
		want string
	}{
		{"/api/user/read", "allow"},
		{"/api/user/write?confirm=1", "deny"},
		{"/other/user/read", ""},
	} {
		rule, ok := matchingEgressRule(rules, &authv3.AttributeContext_HttpRequest{
			Scheme: "https", Host: "example.com", Method: "GET", Path: tt.path,
		})
		if ok != (tt.want != "") || rule.Action != tt.want {
			t.Errorf("matchingEgressRule(%q) = (%q, %v), want %q", tt.path, rule.Action, ok, tt.want)
		}
	}
}

func TestMatchingEgressRuleDenyTakesPrecedence(t *testing.T) {
	rules := []runtimeapi.SandboxEgressRule{
		{Action: "allow", Protocol: "https", Host: "*.example.com", Method: "GET", Path: "/api/**"},
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
	controller := newEgressTestController(map[string]compiledEgressPolicy{
		"10.12.0.2": {
			sandboxID:    "sandbox-allow",
			assignmentID: "10.12.0.2",
			rules:        []runtimeapi.SandboxEgressRule{{Action: "allow", Protocol: "https", Host: "allowed.example.com"}},
		},
		"10.12.0.3": {
			sandboxID:    "sandbox-deny",
			assignmentID: "10.12.0.3",
			rules:        []runtimeapi.SandboxEgressRule{{Action: " deny ", Protocol: "https", Host: "allowed.example.com"}},
		},
	})

	allowed, err := controller.Check(t.Context(), egressCheckRequest("10.12.0.2", "allowed.example.com"))
	if err != nil {
		t.Fatalf("Check(allowed) error = %v", err)
	}
	if allowed.GetStatus().GetCode() != int32(codes.OK) {
		t.Fatalf("Check(allowed) status = %v, want OK", allowed.GetStatus())
	}
	headers := allowed.GetOkResponse().GetHeaders()
	if len(headers) != 1 || headers[0].GetHeader().GetKey() != egressDestinationHeader ||
		headers[0].GetHeader().GetValue() != "198.51.100.1:443" ||
		headers[0].GetAppendAction() != corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD {
		t.Fatalf("Check(allowed) did not set one approved destination: %v", headers)
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

func TestEgressAuthZInternetAccess(t *testing.T) {
	allow, deny := true, false
	for _, tt := range []struct {
		name   string
		access *bool
	}{
		{"omitted", nil},
		{"allowed", &allow},
		{"denied", &deny},
	} {
		t.Run(tt.name, func(t *testing.T) {
			controller := newEgressTestController(map[string]compiledEgressPolicy{
				"10.12.0.2": {
					sandboxID: "sandbox", defaultAllow: tt.access == nil || *tt.access,
					assignmentID: "10.12.0.2",
					rules: []runtimeapi.SandboxEgressRule{
						{Action: "allow", Host: "allowed.example.com"},
						{Action: "deny", Host: "blocked.example.com"},
					},
				},
			})
			for _, host := range []string{"allowed.example.com", "blocked.example.com", "other.example.com"} {
				response, err := controller.Check(t.Context(), egressCheckRequest("10.12.0.2", host))
				if err != nil {
					t.Fatal(err)
				}
				want := codes.PermissionDenied
				if host == "allowed.example.com" || (host == "other.example.com" && (tt.access == nil || *tt.access)) {
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
			defaultAllow: true, denyOut: []string{"198.51.100.1"},
		}, codes.PermissionDenied},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.policy.assignmentID = "10.12.0.2"
			controller := newEgressTestController(map[string]compiledEgressPolicy{"10.12.0.2": tt.policy})
			response, err := controller.Check(t.Context(), egressCheckRequest("10.12.0.2", "example.com"))
			if err != nil || response.GetStatus().GetCode() != int32(tt.want) {
				t.Fatalf("Check() = %v, %v; want %v", response, err, tt.want)
			}
		})
	}
}

func TestEgressAuthZRejectsResolutionFailure(t *testing.T) {
	controller := newEgressTestController(map[string]compiledEgressPolicy{
		"10.12.0.2": {assignmentID: "10.12.0.2", defaultAllow: true},
	})
	request := egressCheckRequest("10.12.0.2", "unresolvable.invalid")
	response, err := controller.Check(t.Context(), request)
	if err != nil || response.GetStatus().GetCode() != int32(codes.Unavailable) || response.GetOkResponse() != nil {
		t.Fatalf("Check() = %v, %v; want Unavailable without an approved destination", response, err)
	}
}

func egressCheckRequest(sourceIP, host string) *authv3.CheckRequest {
	return &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		MetadataContext: &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{
			egressConnectionMetadata: {Fields: map[string]*structpb.Value{
				egressAssignmentKey: structpb.NewStringValue(sourceIP),
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

func TestEgressAuthZRejectsPreviousAssignment(t *testing.T) {
	const sourceIP = "10.12.0.2"
	controller := newEgressTestController(map[string]compiledEgressPolicy{
		sourceIP: {sandboxID: "A", assignmentID: sourceIP, defaultAllow: true},
	})
	connection := egressCheckRequest(sourceIP, "example.com")
	connection.Attributes.Request = nil
	bound, err := controller.Check(t.Context(), connection)
	if err != nil || bound.GetStatus().GetCode() != int32(codes.OK) {
		t.Fatalf("connection authorization = %v, %v", bound, err)
	}
	request := egressCheckRequest(sourceIP, "example.com")
	request.Attributes.MetadataContext.FilterMetadata[egressConnectionMetadata] = bound.GetDynamicMetadata()
	assertStatus := func(want codes.Code) {
		t.Helper()
		response, err := controller.Check(t.Context(), request)
		if err != nil || response.GetStatus().GetCode() != int32(want) {
			t.Fatalf("HTTP authorization = %v, %v; want %v", response, err, want)
		}
	}
	assertStatus(codes.OK)
	value, _ := controller.pool.slotsByIP.Load(sourceIP)
	slot := value.(*Slot)
	if err := controller.Clear(t.Context(), slot); err != nil {
		t.Fatal(err)
	}
	assertStatus(codes.PermissionDenied)
	slot.egressPolicy.Store(&compiledEgressPolicy{sandboxID: "B", assignmentID: "new-assignment", defaultAllow: true})
	controller.pool.slotsByIP.Store(sourceIP, slot)
	assertStatus(codes.PermissionDenied)
	request.Attributes.MetadataContext = nil
	request.Attributes.Request.Http.Headers = map[string]string{egressAssignmentKey: "new-assignment"}
	assertStatus(codes.PermissionDenied)
	request.Attributes.MetadataContext = &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{
		egressConnectionMetadata: {Fields: map[string]*structpb.Value{
			egressAssignmentKey: structpb.NewStringValue("new-assignment"),
		}},
	}}
	assertStatus(codes.OK)
}

func TestClearEgressPolicyKeepsReplacementSlot(t *testing.T) {
	const sourceIP = "10.12.0.2"
	controller := newEgressTestController(map[string]compiledEgressPolicy{
		sourceIP: {sandboxID: "A", assignmentID: "old-assignment"},
	})
	value, _ := controller.pool.slotsByIP.Load(sourceIP)
	old := value.(*Slot)
	replacement := &Slot{cniIP: sourceIP}
	replacement.egressPolicy.Store(&compiledEgressPolicy{sandboxID: "B", assignmentID: "new-assignment"})
	controller.pool.slotsByIP.Store(sourceIP, replacement)
	if err := controller.Clear(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if old.egressPolicy.Load() != nil {
		t.Fatal("cleanup did not invalidate the old policy")
	}
	if current, ok := controller.pool.slotsByIP.Load(sourceIP); !ok || current != replacement {
		t.Fatal("cleanup removed another slot's IP registration")
	}
}

func TestEgressBootstrapBindsSchemeAndConnection(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "config", "envoy", "egress.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := strings.ReplaceAll(string(contents), "\r\n", "\n")
	for _, scheme := range []string{"http", "https"} {
		if !strings.Contains(bootstrap, "scheme_to_overwrite: "+scheme+"\n") {
			t.Fatalf("missing fixed %s scheme", scheme)
		}
	}
	if strings.Count(bootstrap, "- name: envoy.filters.network.ext_authz\n") != 2 ||
		strings.Count(bootstrap, "metadata_context_namespaces: [envoy.filters.network.ext_authz]") != 2 {
		t.Fatal("both handling paths must bind and forward connection ownership")
	}
}

type egressTestResolver struct{}

func newEgressTestController(policies map[string]compiledEgressPolicy) *egressSecurityController {
	controller := &egressSecurityController{resolver: egressTestResolver{}, pool: &Pool{}}
	for ip, policy := range policies {
		slot := &Slot{cniIP: ip}
		slot.egressPolicy.Store(&policy)
		controller.pool.slotsByIP.Store(ip, slot)
	}
	return controller
}

func TestEgressRuntimePathsFollowWorkDir(t *testing.T) {
	workDir := t.TempDir()
	controller, err := newEgressSecurityController(EgressSecurityConfig{
		Enabled: true, WorkDir: workDir,
	}, "cni-conch0")
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(workDir, "egress")
	for _, path := range []string{controller.cfg.AuthzSocket, controller.cfg.AdminSocket, controller.cfg.Bootstrap} {
		if filepath.Dir(path) != wantDir {
			t.Fatalf("runtime path %q is not under %q", path, wantDir)
		}
	}
	if controller.cfg.TLSCertFile != defaultEgressTLSCertFile ||
		controller.cfg.TLSKeyFile != defaultEgressTLSKeyFile ||
		controller.cfg.GuestCAFile != defaultEgressGuestCAFile {
		t.Fatal("egress certificate paths must use the fixed host locations")
	}
}

func (egressTestResolver) LookupIP(ctx context.Context, _, host string) ([]net.IP, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if host == "unresolvable.invalid" {
		return nil, &net.DNSError{Name: host, Err: "test resolution failure", IsNotFound: true}
	}
	return []net.IP{net.ParseIP("198.51.100.1")}, nil
}
