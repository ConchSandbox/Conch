package netstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/coreos/go-iptables/iptables"
)

const (
	sandboxPolicyTable = "filter"
	sandboxPolicyChain = "CONCH-EGRESS"
)

var ErrInvalidSandboxNetworkPolicy = errors.New("invalid sandbox network policy")

// SandboxNetworkAddress is an E2B-compatible egress entry.
//
// The schema keeps entries as strings to preserve the E2B-compatible payload
// shape. Conch's current enforcement accepts only IPv4 addresses and CIDRs.
type SandboxNetworkAddress string

// SandboxNetworkRule preserves E2B-style domain/proxy rule payloads.
type SandboxNetworkRule map[string]any

// SandboxNetworkConfig is the create-time E2B-compatible network schema.
type SandboxNetworkConfig struct {
	AllowPublicTraffic  *bool                   `json:"allowPublicTraffic,omitempty"`
	AllowOut            []SandboxNetworkAddress `json:"allowOut,omitempty"`
	DenyOut             []SandboxNetworkAddress `json:"denyOut,omitempty"`
	EgressProxy         *string                 `json:"egressProxy,omitempty"`
	MaskRequestHost     *bool                   `json:"maskRequestHost,omitempty"`
	Rules               []SandboxNetworkRule    `json:"rules,omitempty"`
	AllowInternetAccess *bool                   `json:"allow_internet_access,omitempty"`
}

// SandboxNetworkUpdateConfig is the backend update schema for a running sandbox.
//
// The public update API is intentionally not wired yet, but this gives the
// internal service/manager layers the same payload shape to build on.
type SandboxNetworkUpdateConfig struct {
	AllowOut            *[]SandboxNetworkAddress `json:"allowOut,omitempty"`
	DenyOut             *[]SandboxNetworkAddress `json:"denyOut,omitempty"`
	EgressProxy         *string                  `json:"egressProxy,omitempty"`
	Rules               []SandboxNetworkRule     `json:"rules,omitempty"`
	AllowInternetAccess *bool                    `json:"allow_internet_access,omitempty"`
}

func ApplySandboxNetworkPolicy(ctx context.Context, slot *Slot, cfg *SandboxNetworkConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateSandboxNetworkPolicy(ctx, cfg); err != nil {
		return err
	}
	if !hasSandboxEgressPolicy(cfg) {
		return nil
	}
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	return runInNetNSPath(slot.NetNSPath(), func() error {
		return applySandboxNetworkPolicyInNS(ctx, cfg)
	})
}

func ReplaceSandboxNetworkPolicy(ctx context.Context, slot *Slot, cfg *SandboxNetworkConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateSandboxNetworkPolicy(ctx, cfg); err != nil {
		return err
	}
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	return runInNetNSPath(slot.NetNSPath(), func() error {
		return applySandboxNetworkPolicyInNS(ctx, cfg)
	})
}

func ClearSandboxNetworkPolicy(ctx context.Context, slot *Slot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if slot == nil {
		return nil
	}
	return runInNetNSPath(slot.NetNSPath(), func() error {
		return clearSandboxNetworkPolicyInNS()
	})
}

func applySandboxNetworkPolicyInNS(ctx context.Context, cfg *SandboxNetworkConfig) error {
	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("error initializing iptables: %w", err)
	}
	if err := ensureSandboxPolicyChain(tables); err != nil {
		return err
	}
	if err := tables.ClearChain(sandboxPolicyTable, sandboxPolicyChain); err != nil {
		return fmt.Errorf("clear sandbox network policy chain: %w", err)
	}
	if cfg == nil {
		return nil
	}

	for _, dst := range policyDestinations(cfg.DenyOut) {
		if err := tables.Append(sandboxPolicyTable, sandboxPolicyChain, "-d", dst, "-j", "REJECT"); err != nil {
			return fmt.Errorf("append denyOut rule for %s: %w", dst, err)
		}
	}

	allow := policyDestinations(cfg.AllowOut)
	for _, dst := range allow {
		if err := tables.Append(sandboxPolicyTable, sandboxPolicyChain, "-d", dst, "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("append allowOut rule for %s: %w", dst, err)
		}
	}
	if len(allow) > 0 || (cfg.AllowInternetAccess != nil && !*cfg.AllowInternetAccess) {
		if err := tables.Append(sandboxPolicyTable, sandboxPolicyChain, "-j", "REJECT"); err != nil {
			return fmt.Errorf("append default egress reject rule: %w", err)
		}
	}
	return nil
}

func hasSandboxEgressPolicy(cfg *SandboxNetworkConfig) bool {
	return cfg != nil && (len(cfg.AllowOut) > 0 || len(cfg.DenyOut) > 0 || cfg.AllowInternetAccess != nil)
}

func ValidateSandboxNetworkPolicy(ctx context.Context, cfg *SandboxNetworkConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	for _, check := range []struct {
		field   string
		entries []SandboxNetworkAddress
	}{
		{field: "allowOut", entries: cfg.AllowOut},
		{field: "denyOut", entries: cfg.DenyOut},
	} {
		for _, entry := range check.entries {
			if _, ok := normalizePolicyDestination(string(entry)); !ok {
				return fmt.Errorf("%w: %s contains unsupported destination %q; only IPv4 addresses and CIDRs are supported", ErrInvalidSandboxNetworkPolicy, check.field, string(entry))
			}
		}
	}
	return nil
}

func clearSandboxNetworkPolicyInNS() error {
	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("error initializing iptables: %w", err)
	}
	exists, err := tables.ChainExists(sandboxPolicyTable, sandboxPolicyChain)
	if err != nil {
		return fmt.Errorf("check sandbox network policy chain: %w", err)
	}
	if !exists {
		return nil
	}
	return tables.ClearChain(sandboxPolicyTable, sandboxPolicyChain)
}

func ensureSandboxPolicyChain(tables *iptables.IPTables) error {
	exists, err := tables.ChainExists(sandboxPolicyTable, sandboxPolicyChain)
	if err != nil {
		return fmt.Errorf("check sandbox network policy chain: %w", err)
	}
	if !exists {
		if err := tables.NewChain(sandboxPolicyTable, sandboxPolicyChain); err != nil {
			return fmt.Errorf("create sandbox network policy chain: %w", err)
		}
	}
	for _, chain := range []string{"OUTPUT", "FORWARD"} {
		if err := tables.DeleteIfExists(sandboxPolicyTable, chain, "-j", sandboxPolicyChain); err != nil {
			return fmt.Errorf("remove stale sandbox network policy jump on %s: %w", chain, err)
		}
		if err := tables.Insert(sandboxPolicyTable, chain, 1, "-j", sandboxPolicyChain); err != nil {
			return fmt.Errorf("install sandbox network policy jump on %s: %w", chain, err)
		}
	}
	return nil
}

func policyDestinations(entries []SandboxNetworkAddress) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if dst, ok := normalizePolicyDestination(string(entry)); ok {
			out = append(out, dst)
		}
	}
	return out
}

func normalizePolicyDestination(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if ip := net.ParseIP(raw); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), true
		}
		return "", false
	}
	if _, ipNet, err := net.ParseCIDR(raw); err == nil {
		if ipNet.IP.To4() == nil {
			return "", false
		}
		return ipNet.String(), true
	}
	return "", false
}
