package netstack

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/runtimeapi"
)

type fakePolicyIPTables struct {
	appends     [][]string
	inserts     [][]string
	appendCalls int
	failAppend  int
	activeChain string
	switches    [][]string
	switchErr   error
}

func (f *fakePolicyIPTables) Append(table, chain string, rules ...string) error {
	f.appendCalls++
	f.appends = append(f.appends, append([]string(nil), rules...))
	if f.appendCalls == f.failAppend {
		return fmt.Errorf("append failed")
	}
	return nil
}

func (*fakePolicyIPTables) ChainExists(string, string) (bool, error) { return false, nil }
func (*fakePolicyIPTables) ClearChain(string, string) error          { return nil }
func (*fakePolicyIPTables) DeleteIfExists(string, string, ...string) error {
	return nil
}
func (f *fakePolicyIPTables) Exists(_ string, chain string, rules ...string) (bool, error) {
	return chain == "OUTPUT" && len(rules) == 2 && rules[0] == "-j" && rules[1] == f.activeChain, nil
}
func (f *fakePolicyIPTables) Insert(_ string, chain string, _ int, rules ...string) error {
	f.inserts = append(f.inserts, append([]string{chain}, rules...))
	return nil
}
func (*fakePolicyIPTables) NewChain(string, string) error { return nil }

func useFakePolicyIPTables(t *testing.T, tables sandboxPolicyIPTables) {
	t.Helper()
	original := newSandboxPolicyIPTables
	originalSwitch := switchSandboxPolicyChain
	newSandboxPolicyIPTables = func() (sandboxPolicyIPTables, error) {
		return tables, nil
	}
	switchSandboxPolicyChain = func(active, target, tapName string) error {
		if fake, ok := tables.(*fakePolicyIPTables); ok {
			fake.switches = append(fake.switches, []string{active, target, tapName})
			return fake.switchErr
		}
		return nil
	}
	t.Cleanup(func() {
		newSandboxPolicyIPTables = original
		switchSandboxPolicyChain = originalSwitch
	})
}

func TestNormalizePolicyDestination(t *testing.T) {
	for _, tt := range []struct {
		raw string
		ok  bool
	}{
		{raw: "10.0.0.1", ok: true},
		{raw: "10.0.0.0/8", ok: true},
		{raw: "example.com", ok: false},
		{raw: "", ok: false},
	} {
		if _, ok := normalizePolicyDestination(tt.raw); ok != tt.ok {
			t.Fatalf("normalizePolicyDestination(%q) ok = %v, want %v", tt.raw, ok, tt.ok)
		}
	}
}

func TestValidateSandboxNetworkPolicy(t *testing.T) {
	ctx := context.Background()
	if err := ValidateSandboxNetworkPolicy(ctx, &runtimeapi.SandboxNetworkConfig{
		AllowOut:    []string{"10.0.0.1", "10.0.0.0/8"},
		DenyOut:     []string{"192.0.2.1"},
		EgressProxy: &runtimeapi.SandboxEgressProxyConfig{},
		Rules:       map[string]any{},
	}); err != nil {
		t.Fatalf("ValidateSandboxNetworkPolicy() error = %v", err)
	}

	for _, tt := range []struct {
		name  string
		cfg   *runtimeapi.SandboxNetworkConfig
		field string
	}{
		{name: "empty allow", cfg: &runtimeapi.SandboxNetworkConfig{AllowOut: []string{""}}, field: "allowOut"},
		{name: "invalid deny", cfg: &runtimeapi.SandboxNetworkConfig{DenyOut: []string{"%%%"}}, field: "denyOut"},
		{name: "domain allow", cfg: &runtimeapi.SandboxNetworkConfig{AllowOut: []string{"example.com"}}, field: "allowOut"},
		{name: "non-empty proxy address", cfg: &runtimeapi.SandboxNetworkConfig{EgressProxy: &runtimeapi.SandboxEgressProxyConfig{Address: "http://proxy.example"}}, field: "egressProxy"},
		{name: "non-empty proxy credentials", cfg: &runtimeapi.SandboxNetworkConfig{EgressProxy: &runtimeapi.SandboxEgressProxyConfig{Username: "user"}}, field: "egressProxy"},
		{name: "non-empty rules", cfg: &runtimeapi.SandboxNetworkConfig{Rules: map[string]any{"rule": true}}, field: "rules"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSandboxNetworkPolicy(ctx, tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("ValidateSandboxNetworkPolicy() error = %v, want field %s", err, tt.field)
			}
			if !errors.Is(err, ErrInvalidSandboxNetworkPolicy) {
				t.Fatalf("ValidateSandboxNetworkPolicy() error = %v, want ErrInvalidSandboxNetworkPolicy", err)
			}
		})
	}
}

func TestSandboxPolicyAllowsReturnTrafficBeforeEgressFiltering(t *testing.T) {
	tables := &fakePolicyIPTables{}
	useFakePolicyIPTables(t, tables)
	allowInternet := false

	if err := applySandboxNetworkPolicyInNS(&runtimeapi.SandboxNetworkConfig{
		AllowOut:            []string{"192.0.2.1"},
		DenyOut:             []string{"198.51.100.1"},
		AllowInternetAccess: &allowInternet,
	}, tapInterfaceName); err != nil {
		t.Fatalf("applySandboxNetworkPolicyInNS() error = %v", err)
	}

	wantFirst := []string{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"}
	if len(tables.appends) == 0 || !reflect.DeepEqual(tables.appends[0], wantFirst) {
		t.Fatalf("first policy rule = %#v, want %#v", tables.appends, wantFirst)
	}
	if got := tables.appends[len(tables.appends)-1]; !reflect.DeepEqual(got, []string{"-j", "REJECT"}) {
		t.Fatalf("last policy rule = %#v, want default reject", got)
	}
	wantSwitch := []string{"", sandboxPolicyA, tapInterfaceName}
	if len(tables.switches) != 1 || !reflect.DeepEqual(tables.switches[0], wantSwitch) {
		t.Fatalf("policy switch = %#v, want %#v", tables.switches, wantSwitch)
	}
}

func TestSandboxPolicyReturnsPartialAppendFailure(t *testing.T) {
	tables := &fakePolicyIPTables{failAppend: 2}
	useFakePolicyIPTables(t, tables)

	err := applySandboxNetworkPolicyInNS(&runtimeapi.SandboxNetworkConfig{
		DenyOut: []string{"198.51.100.1"},
	}, tapInterfaceName)
	if err == nil || !strings.Contains(err.Error(), "append denyOut rule") {
		t.Fatalf("applySandboxNetworkPolicyInNS() error = %v, want denyOut append failure", err)
	}
	if tables.appendCalls != 2 {
		t.Fatalf("Append calls = %d, want stop after first failed policy rule", tables.appendCalls)
	}
	if len(tables.switches) != 0 {
		t.Fatalf("policy switched after partial build: %#v", tables.switches)
	}
}

func TestSandboxPolicyKeepsActiveChainWhenSwitchFails(t *testing.T) {
	tables := &fakePolicyIPTables{
		activeChain: sandboxPolicyA,
		switchErr:   errors.New("restore failed"),
	}
	useFakePolicyIPTables(t, tables)

	err := applySandboxNetworkPolicyInNS(&runtimeapi.SandboxNetworkConfig{
		DenyOut: []string{"198.51.100.1"},
	}, tapInterfaceName)
	if err == nil || !strings.Contains(err.Error(), "switch sandbox network policy chain") {
		t.Fatalf("applySandboxNetworkPolicyInNS() error = %v, want switch failure", err)
	}
	wantSwitch := []string{sandboxPolicyA, sandboxPolicyB, tapInterfaceName}
	if len(tables.switches) != 1 || !reflect.DeepEqual(tables.switches[0], wantSwitch) {
		t.Fatalf("policy switch = %#v, want %#v", tables.switches, wantSwitch)
	}
	if tables.activeChain != sandboxPolicyA {
		t.Fatalf("active chain = %q, want preserved %q", tables.activeChain, sandboxPolicyA)
	}
}
