package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// E2BConfig enables the private TCP Node API consumed by AgentENV Gateway.
// The key is resolved only from AENV_API_KEY and is not a YAML option.
type E2BConfig struct {
	ListenAddr          string   `yaml:"listen_addr"`
	SandboxProxyDomains []string `yaml:"sandbox_proxy_domains"`
	APIKey              string   `yaml:"-"`
}

// ClusterConfig identifies this Node to the unmodified AgentENV Scheduler.
// An empty SchedulerAddr disables reporting for a standalone Node.
type ClusterConfig struct {
	SchedulerAddr string `yaml:"scheduler_addr"`
	NodeID        string `yaml:"node_id"`
	ClusterID     string `yaml:"cluster_id"`
}

func (cfg *Config) applyE2BEnvironment() {
	cfg.E2B.APIKey = os.Getenv("AENV_API_KEY")
}

func validateE2BConfig(cfg *Config) error {
	e2b := &cfg.E2B
	cluster := &cfg.Cluster
	e2b.ListenAddr = strings.TrimSpace(e2b.ListenAddr)
	e2b.APIKey = strings.TrimSpace(e2b.APIKey)
	cluster.SchedulerAddr = strings.TrimSpace(cluster.SchedulerAddr)
	cluster.NodeID = strings.TrimSpace(cluster.NodeID)
	cluster.ClusterID = strings.TrimSpace(cluster.ClusterID)
	domains := make([]string, 0, len(e2b.SandboxProxyDomains))
	for i, domain := range e2b.SandboxProxyDomains {
		domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
		if domain == "" {
			continue
		}
		if err := validateProxyDomain(domain); err != nil {
			return fmt.Errorf("invalid e2b.sandbox_proxy_domains[%d]: %w", i, err)
		}
		domains = append(domains, domain)
	}
	e2b.SandboxProxyDomains = domains
	if e2b.ListenAddr != "" {
		if cfg.Sandbox.RequestTimeout <= 0 {
			return fmt.Errorf("sandbox.request_timeout must be positive when E2B is enabled")
		}
		if err := validateTCPAddress(e2b.ListenAddr, true); err != nil {
			return fmt.Errorf("invalid e2b.listen_addr: %w", err)
		}
		if e2b.APIKey == "" {
			return fmt.Errorf("AENV_API_KEY is required when the E2B listener is enabled")
		}
		if strings.ContainsAny(e2b.APIKey, "\r\n") {
			return fmt.Errorf("AENV_API_KEY must not contain line breaks")
		}
	}
	if cluster.SchedulerAddr != "" {
		addr := cluster.SchedulerAddr
		if strings.Contains(addr, "://") {
			u, err := url.Parse(addr)
			if err != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("cluster.scheduler_addr must be a plaintext host:port or http://host:port endpoint")
			}
			addr = u.Host
		}
		if err := validateTCPAddress(addr, false); err != nil {
			return fmt.Errorf("invalid cluster.scheduler_addr: %w", err)
		}
		cluster.SchedulerAddr = addr
		if e2b.ListenAddr == "" || cluster.NodeID == "" || cluster.ClusterID == "" {
			return fmt.Errorf("cluster.scheduler_addr requires e2b.listen_addr, cluster.node_id and cluster.cluster_id")
		}
	}
	return nil
}

func validateTCPAddress(addr string, listen bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || (!listen && strings.TrimSpace(host) == "") {
		return fmt.Errorf("address must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("TCP port must be between 1 and 65535")
	}
	if strings.ContainsAny(host, " \t\r\n/@?#") {
		return fmt.Errorf("invalid TCP hostname")
	}
	return nil
}

func validateProxyDomain(domain string) error {
	if domain == "" || strings.ContainsAny(domain, ":/@?# \t\r\n") {
		return fmt.Errorf("expected a bare DNS domain without scheme, port or path")
	}
	host := domain
	// Match AgentENV Gateway's bare DNS suffix validation. Ports belong to the
	// Gateway transport endpoint, not to sandbox_proxy_domains.
	if len(host) > 253 {
		return fmt.Errorf("proxy domain must be a DNS suffix")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("invalid DNS label")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '-' {
				return fmt.Errorf("invalid DNS label")
			}
		}
	}
	return nil
}
