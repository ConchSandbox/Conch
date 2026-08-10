package netstack

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestValidateSandboxNetworkInputConfig(t *testing.T) {
	if err := ValidateSandboxNetworkInputConfig(context.Background(), &SandboxNetworkConfig{
		AllowOut: []string{"192.0.2.10", "198.51.100.0/24"},
		DenyIn:   []string{"203.0.113.7"},
	}); err != nil {
		t.Fatalf("ValidateSandboxNetworkInputConfig() error = %v", err)
	}
	for _, destination := range []string{"example.com", "2001:db8::1", "not-an-address"} {
		err := ValidateSandboxNetworkInputConfig(context.Background(), &SandboxNetworkConfig{AllowOut: []string{destination}})
		if err == nil || !strings.Contains(err.Error(), destination) {
			t.Fatalf("ValidateSandboxNetworkInputConfig(%q) error = %v", destination, err)
		}
	}
}

func TestBuildPolicyChainOrderingAndDefaults(t *testing.T) {
	allowInternet := false
	config := &SandboxNetworkConfig{
		AllowOut:            []string{"192.0.2.10"},
		DenyOut:             []string{"192.0.2.11"},
		AllowIn:             []string{"198.51.100.10"},
		DenyIn:              []string{"198.51.100.11"},
		AllowInternetAccess: &allowInternet,
	}

	wantEgress := [][]string{
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-d", "192.0.2.11", "-j", "REJECT"},
		{"-d", "192.0.2.10", "-j", "ACCEPT"},
		{"-j", "REJECT"},
	}
	wantIngress := [][]string{
		{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-s", "198.51.100.11", "-j", "REJECT"},
		{"-s", "198.51.100.10", "-j", "ACCEPT"},
		{"-j", "REJECT"},
	}
	if got := sandboxPolicyRules(config, false); !reflect.DeepEqual(got, wantEgress) {
		t.Fatalf("egress rules = %v, want %v", got, wantEgress)
	}
	if got := sandboxPolicyRules(config, true); !reflect.DeepEqual(got, wantIngress) {
		t.Fatalf("ingress rules = %v, want %v", got, wantIngress)
	}
}

func TestValidateSandboxNetworkInputConfigLimitsDestinations(t *testing.T) {
	entries := make([]string, MaxSandboxNetworkDestinations+1)
	for i := range entries {
		entries[i] = fmt.Sprintf("10.%d.%d.%d", i>>16, i>>8&255, i&255)
	}
	if err := ValidateSandboxNetworkInputConfig(context.Background(), &SandboxNetworkConfig{AllowOut: entries}); err == nil {
		t.Fatal("ValidateSandboxNetworkInputConfig() accepted an oversized policy")
	}
}

func TestRestoreSandboxNetworkPolicyUsesDirectionalHooks(t *testing.T) {
	original := runSandboxPolicyRestore
	t.Cleanup(func() { runSandboxPolicyRestore = original })
	var got string
	runSandboxPolicyRestore = func(_ context.Context, rules string) error {
		got = rules
		return nil
	}

	if err := applySandboxNetworkPolicyBatch(context.Background(), "tap0", true, true, &SandboxNetworkConfig{}); err != nil {
		t.Fatalf("applySandboxNetworkPolicyBatch() error = %v", err)
	}
	for _, rule := range []string{
		"-F CONCH-EGRESS",
		"-F CONCH-INGRESS",
		"-I FORWARD 1 -i tap0 -j CONCH-EGRESS",
		"-I FORWARD 1 -o tap0 -j CONCH-INGRESS",
	} {
		if !strings.Contains(got, rule) {
			t.Fatalf("restore rules %q do not contain %q", got, rule)
		}
	}
}
