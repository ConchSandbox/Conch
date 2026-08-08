package netstack

import (
	"fmt"
	"net"
	"strings"
	"unicode"
)

const maxNameservers = 3

// NormalizeDNS validates and canonicalizes DNS configuration returned by CNI.
func NormalizeDNS(cfg DNSConfig) (DNSConfig, error) {
	var out DNSConfig
	for _, raw := range cfg.Nameservers {
		ip := net.ParseIP(strings.TrimSpace(raw))
		if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() {
			return DNSConfig{}, fmt.Errorf("nameserver %q must be a reachable IPv4 address", raw)
		}
		out.Nameservers = append(out.Nameservers, ip.To4().String())
	}
	out.Nameservers = uniqueLimited(out.Nameservers, maxNameservers)

	if cfg.Domain != "" {
		if !validResolverToken(cfg.Domain) {
			return DNSConfig{}, fmt.Errorf("invalid domain %q", cfg.Domain)
		}
		out.Domain = cfg.Domain
	}
	for _, value := range cfg.Search {
		if !validResolverToken(value) {
			return DNSConfig{}, fmt.Errorf("invalid search domain %q", value)
		}
		out.Search = append(out.Search, value)
	}
	out.Search = uniqueLimited(out.Search, 0)
	for _, value := range cfg.Options {
		if !validResolverToken(value) {
			return DNSConfig{}, fmt.Errorf("invalid resolver option %q", value)
		}
		out.Options = append(out.Options, value)
	}
	out.Options = uniqueLimited(out.Options, 0)
	if len(out.Search) != 0 {
		out.Domain = ""
	}
	return out, nil
}

func validResolverToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func uniqueLimited(values []string, limit int) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}
