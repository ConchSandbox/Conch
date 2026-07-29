package netstack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/coreos/go-iptables/iptables"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

const (
	sandboxPolicyTable = "filter"
	sandboxPolicyChain = "CONCH-EGRESS"
	sandboxPolicyA     = "CONCH-EGRESS-A"
	sandboxPolicyB     = "CONCH-EGRESS-B"
)

var ErrInvalidSandboxNetworkPolicy = errors.New("invalid sandbox network policy")

type sandboxPolicyIPTables interface {
	Append(string, string, ...string) error
	ChainExists(string, string) (bool, error)
	ClearChain(string, string) error
	DeleteIfExists(string, string, ...string) error
	Exists(string, string, ...string) (bool, error)
	Insert(string, string, int, ...string) error
	NewChain(string, string) error
}

var newSandboxPolicyIPTables = func() (sandboxPolicyIPTables, error) {
	return iptables.New()
}

var switchSandboxPolicyChain = switchSandboxPolicyChainWithRestore

func ApplySandboxNetworkPolicy(ctx context.Context, slot *Slot, cfg *runtimeapi.SandboxNetworkConfig) error {
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
		return applySandboxNetworkPolicyInNS(cfg, slot.TapName())
	})
}

func ReplaceSandboxNetworkPolicy(ctx context.Context, slot *Slot, cfg *runtimeapi.SandboxNetworkConfig) error {
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
		return applySandboxNetworkPolicyInNS(cfg, slot.TapName())
	})
}

func ClearSandboxNetworkPolicy(ctx context.Context, slot *Slot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if slot == nil {
		return nil
	}
	return runInNetNSPath(slot.NetNSPath(), clearSandboxNetworkPolicyInNS)
}

func applySandboxNetworkPolicyInNS(cfg *runtimeapi.SandboxNetworkConfig, tapName string) error {
	tables, err := newSandboxPolicyIPTables()
	if err != nil {
		return fmt.Errorf("error initializing iptables: %w", err)
	}
	active, err := activeSandboxPolicyChain(tables)
	if err != nil {
		return err
	}
	target := sandboxPolicyA
	if active == sandboxPolicyA {
		target = sandboxPolicyB
	}
	if err := buildSandboxPolicyChain(tables, target, cfg); err != nil {
		return err
	}
	if err := switchSandboxPolicyChain(active, target, tapName); err != nil {
		return fmt.Errorf("switch sandbox network policy chain: %w", err)
	}
	return nil
}

func buildSandboxPolicyChain(tables sandboxPolicyIPTables, chain string, cfg *runtimeapi.SandboxNetworkConfig) error {
	exists, err := tables.ChainExists(sandboxPolicyTable, chain)
	if err != nil {
		return fmt.Errorf("check sandbox network policy chain %s: %w", chain, err)
	}
	if !exists {
		if err := tables.NewChain(sandboxPolicyTable, chain); err != nil {
			return fmt.Errorf("create sandbox network policy chain %s: %w", chain, err)
		}
	}
	if err := tables.ClearChain(sandboxPolicyTable, chain); err != nil {
		return fmt.Errorf("clear sandbox network policy chain %s: %w", chain, err)
	}
	if cfg == nil {
		return nil
	}
	if err := tables.Append(sandboxPolicyTable, chain, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		return fmt.Errorf("append established egress rule: %w", err)
	}
	for _, dst := range policyDestinations(cfg.DenyOut) {
		if err := tables.Append(sandboxPolicyTable, chain, "-d", dst, "-j", "REJECT"); err != nil {
			return fmt.Errorf("append denyOut rule for %s: %w", dst, err)
		}
	}
	allow := policyDestinations(cfg.AllowOut)
	for _, dst := range allow {
		if err := tables.Append(sandboxPolicyTable, chain, "-d", dst, "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("append allowOut rule for %s: %w", dst, err)
		}
	}
	if len(allow) > 0 || (cfg.AllowInternetAccess != nil && !*cfg.AllowInternetAccess) {
		if err := tables.Append(sandboxPolicyTable, chain, "-j", "REJECT"); err != nil {
			return fmt.Errorf("append default egress reject rule: %w", err)
		}
	}
	return nil
}

func activeSandboxPolicyChain(tables sandboxPolicyIPTables) (string, error) {
	for _, chain := range []string{sandboxPolicyA, sandboxPolicyB, sandboxPolicyChain} {
		exists, err := tables.Exists(sandboxPolicyTable, "OUTPUT", "-j", chain)
		if err != nil {
			return "", fmt.Errorf("check active sandbox network policy chain %s: %w", chain, err)
		}
		if exists {
			return chain, nil
		}
	}
	return "", nil
}

func switchSandboxPolicyChainWithRestore(active, target, tapName string) error {
	var rules strings.Builder
	rules.WriteString("*filter\n")
	fmt.Fprintf(&rules, "-I OUTPUT 1 -j %s\n", target)
	fmt.Fprintf(&rules, "-I FORWARD 1 -i %s -j %s\n", tapName, target)
	if active != "" {
		fmt.Fprintf(&rules, "-D OUTPUT -j %s\n", active)
		fmt.Fprintf(&rules, "-D FORWARD -i %s -j %s\n", tapName, active)
	}
	rules.WriteString("COMMIT\n")

	cmd := exec.Command("iptables-restore", "--noflush", "--wait")
	cmd.Stdin = strings.NewReader(rules.String())
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("iptables-restore failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func hasSandboxEgressPolicy(cfg *runtimeapi.SandboxNetworkConfig) bool {
	return cfg != nil && (len(cfg.AllowOut) > 0 || len(cfg.DenyOut) > 0 || cfg.AllowInternetAccess != nil)
}

func ValidateSandboxNetworkPolicy(ctx context.Context, cfg *runtimeapi.SandboxNetworkConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	if cfg.EgressProxy != nil && (strings.TrimSpace(cfg.EgressProxy.Address) != "" ||
		strings.TrimSpace(cfg.EgressProxy.Username) != "" ||
		strings.TrimSpace(cfg.EgressProxy.Password) != "") {
		return fmt.Errorf("%w: egressProxy is not supported", ErrInvalidSandboxNetworkPolicy)
	}
	if len(cfg.Rules) != 0 {
		return fmt.Errorf("%w: rules are not supported", ErrInvalidSandboxNetworkPolicy)
	}
	for _, check := range []struct {
		field   string
		entries []string
	}{
		{field: "allowOut", entries: cfg.AllowOut},
		{field: "denyOut", entries: cfg.DenyOut},
	} {
		for _, entry := range check.entries {
			if _, ok := normalizePolicyDestination(entry); !ok {
				return fmt.Errorf("%w: %s contains unsupported destination %q; only IPv4 addresses and CIDRs are supported", ErrInvalidSandboxNetworkPolicy, check.field, entry)
			}
		}
	}
	return nil
}

func clearSandboxNetworkPolicyInNS() error {
	tables, err := newSandboxPolicyIPTables()
	if err != nil {
		return fmt.Errorf("error initializing iptables: %w", err)
	}
	for _, chain := range []string{sandboxPolicyA, sandboxPolicyB, sandboxPolicyChain} {
		exists, err := tables.ChainExists(sandboxPolicyTable, chain)
		if err != nil {
			return fmt.Errorf("check sandbox network policy chain %s: %w", chain, err)
		}
		if exists {
			if err := tables.ClearChain(sandboxPolicyTable, chain); err != nil {
				return fmt.Errorf("clear sandbox network policy chain %s: %w", chain, err)
			}
		}
	}
	return nil
}

func policyDestinations(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if dst, ok := normalizePolicyDestination(entry); ok {
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
