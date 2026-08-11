package netstack

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/openeuler/Conch/internal/runtimeapi"
)

const (
	sandboxPolicyTable = "filter"
	egressPolicyChain  = "CONCH-EGRESS"
	ingressPolicyChain = "CONCH-INGRESS"

	MaxSandboxNetworkDestinations = 1024
	sandboxPolicyLockTimeout      = 5 * time.Minute
)

type SandboxNetworkConfig = runtimeapi.SandboxNetworkConfig

type sandboxPolicyIPTables interface {
	ChainExists(string, string) (bool, error)
	ClearChain(string, string) error
	DeleteIfExists(string, string, ...string) error
	Exists(string, string, ...string) (bool, error)
	NewChain(string, string) error
}

var newSandboxPolicyIPTables = func(timeout time.Duration) (sandboxPolicyIPTables, error) {
	return iptables.New(iptables.Timeout(int(timeout / time.Second)))
}

var runSandboxPolicyRestore = func(ctx context.Context, rules string) error {
	cmd := exec.CommandContext(ctx, "iptables-restore", "--noflush", "--wait")
	cmd.Stdin = strings.NewReader(rules)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("switch sandbox network policy chains: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func ValidateSandboxNetworkInputConfig(ctx context.Context, cfg *SandboxNetworkConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	total := len(cfg.AllowOut) + len(cfg.DenyOut) + len(cfg.AllowIn) + len(cfg.DenyIn)
	if total > MaxSandboxNetworkDestinations {
		return fmt.Errorf("%w: at most %d destinations are supported", ErrInvalidPolicy, MaxSandboxNetworkDestinations)
	}
	for _, field := range []struct {
		name    string
		entries []string
	}{
		{name: "allowOut", entries: cfg.AllowOut},
		{name: "denyOut", entries: cfg.DenyOut},
		{name: "allowIn", entries: cfg.AllowIn},
		{name: "denyIn", entries: cfg.DenyIn},
	} {
		for _, entry := range field.entries {
			if _, ok := normalizePolicyDestination(entry); !ok {
				return fmt.Errorf("%w: %s contains unsupported destination %q; only IPv4 addresses and CIDRs are supported", ErrInvalidPolicy, field.name, entry)
			}
		}
	}
	return nil
}

func isNetworkConfigNonEmpty(cfg *SandboxNetworkConfig) bool {
	return cfg != nil && (len(cfg.AllowOut) != 0 || len(cfg.DenyOut) != 0 ||
		len(cfg.AllowIn) != 0 || len(cfg.DenyIn) != 0 ||
		(cfg.AllowInternetAccess != nil && !*cfg.AllowInternetAccess))
}

func writeSandboxNetworkPolicyRules(ctx context.Context, slot *Slot, cfg *SandboxNetworkConfig) error {
	if slot == nil {
		return fmt.Errorf("slot is nil")
	}
	return runInNetNSPath(ctx, slot.NetNSPath(), func() error {
		tables, err := newSandboxPolicyIPTables(sandboxPolicyLockTimeout)
		if err != nil {
			return fmt.Errorf("initialize iptables: %w", err)
		}
		for _, chain := range []string{egressPolicyChain, ingressPolicyChain} {
			exists, existsErr := tables.ChainExists(sandboxPolicyTable, chain)
			if existsErr != nil {
				return fmt.Errorf("check policy chain %s: %w", chain, existsErr)
			}
			if !exists {
				if err := tables.NewChain(sandboxPolicyTable, chain); err != nil {
					return fmt.Errorf("create policy chain %s: %w", chain, err)
				}
			}
		}
		egressHook, err := tables.Exists(sandboxPolicyTable, "FORWARD", "-i", slot.TapName(), "-j", egressPolicyChain)
		if err != nil {
			return fmt.Errorf("check egress policy hook: %w", err)
		}
		ingressHook, err := tables.Exists(sandboxPolicyTable, "FORWARD", "-o", slot.TapName(), "-j", ingressPolicyChain)
		if err != nil {
			return fmt.Errorf("check ingress policy hook: %w", err)
		}
		return applySandboxNetworkPolicyBatch(ctx, slot.TapName(), !egressHook, !ingressHook, cfg)
	})
}

func applySandboxNetworkPolicyBatch(ctx context.Context, tapName string, addEgressHook, addIngressHook bool, cfg *SandboxNetworkConfig) error {
	var rules strings.Builder
	rules.WriteString("*filter\n")
	fmt.Fprintf(&rules, "-F %s\n", egressPolicyChain)
	for _, rule := range sandboxPolicyRules(cfg, false) {
		fmt.Fprintf(&rules, "-A %s %s\n", egressPolicyChain, strings.Join(rule, " "))
	}
	fmt.Fprintf(&rules, "-F %s\n", ingressPolicyChain)
	for _, rule := range sandboxPolicyRules(cfg, true) {
		fmt.Fprintf(&rules, "-A %s %s\n", ingressPolicyChain, strings.Join(rule, " "))
	}
	if addEgressHook {
		fmt.Fprintf(&rules, "-I FORWARD 1 -i %s -j %s\n", tapName, egressPolicyChain)
	}
	if addIngressHook {
		fmt.Fprintf(&rules, "-I FORWARD 1 -o %s -j %s\n", tapName, ingressPolicyChain)
	}
	rules.WriteString("COMMIT\n")
	return runSandboxPolicyRestore(ctx, rules.String())
}

func sandboxPolicyRules(cfg *SandboxNetworkConfig, ingress bool) [][]string {
	rules := [][]string{{"-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"}}
	if cfg == nil {
		return rules
	}
	deny, allow, addressFlag := cfg.DenyOut, cfg.AllowOut, "-d"
	defaultReject := cfg.AllowInternetAccess != nil && !*cfg.AllowInternetAccess
	if ingress {
		deny, allow, addressFlag = cfg.DenyIn, cfg.AllowIn, "-s"
		defaultReject = false
	}
	for entry := range policyDestinations(deny) {
		rules = append(rules, []string{addressFlag, entry, "-j", "REJECT"})
	}
	for entry := range policyDestinations(allow) {
		rules = append(rules, []string{addressFlag, entry, "-j", "ACCEPT"})
	}
	if len(allow) != 0 || defaultReject {
		rules = append(rules, []string{"-j", "REJECT"})
	}
	return rules
}

func policyDestinations(entries []string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for _, entry := range entries {
			if normalized, ok := normalizePolicyDestination(entry); ok && !yield(normalized) {
				return
			}
		}
	}
}

func normalizePolicyDestination(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if ip := net.ParseIP(raw); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), true
		}
		return "", false
	}
	_, network, err := net.ParseCIDR(raw)
	if err != nil || network.IP.To4() == nil {
		return "", false
	}
	return network.String(), true
}

func clearSandboxNetworkPolicyRules(ctx context.Context, slot *Slot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if slot == nil {
		return nil
	}
	return runInNetNSPath(ctx, slot.NetNSPath(), func() error {
		tables, err := newSandboxPolicyIPTables(sandboxPolicyLockTimeout)
		if err != nil {
			return fmt.Errorf("initialize iptables: %w", err)
		}
		chains := []struct {
			name string
			hook []string
		}{
			{name: egressPolicyChain, hook: []string{"-i", slot.TapName(), "-j", egressPolicyChain}},
			{name: ingressPolicyChain, hook: []string{"-o", slot.TapName(), "-j", ingressPolicyChain}},
		}

		var errs []error
		for _, chain := range chains {
			exists, existsErr := tables.ChainExists(sandboxPolicyTable, chain.name)
			if existsErr != nil {
				errs = append(errs, existsErr)
				continue
			}
			if !exists {
				continue
			}
			errs = append(errs, tables.DeleteIfExists(sandboxPolicyTable, "FORWARD", chain.hook...))
			errs = append(errs, tables.ClearChain(sandboxPolicyTable, chain.name))
		}
		return errors.Join(errs...)
	})
}
