package netstack

import (
	"context"
	"fmt"
	"net"

	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/vishvananda/netlink"
)

const (
	DefaultCNIPluginConfDir = "/etc/conch/cni/net.d"
	DefaultCNIPluginBinDir  = "/usr/libexec/cni"

	defaultCNIPluginConfDir = DefaultCNIPluginConfDir
	defaultCNIPluginBinDir  = DefaultCNIPluginBinDir
	cniOuterInterfaceName   = "eth0"
	defaultCNICacheDir      = "/var/lib/conch/cni"
	cniCacheDir             = defaultCNICacheDir
)

type CNIManagerConfig struct {
	PluginBinDirs []string `toml:"plugin_bin_dirs" json:"pluginBinDirs" yaml:"plugin_bin_dirs"`
	PluginConfDir string   `toml:"-" json:"-" yaml:"-"`
	CacheDir      string   `toml:"-" json:"-" yaml:"-"`
}

type cniAttachment struct {
	ContainerID   string
	NetworkName   string
	InterfaceName string
	NetNS         string
}

type cniBackend interface {
	Setup(context.Context, string, string) (*types100.Result, error)
	Remove(context.Context, string, string) error
	CachedAttachments() ([]cniAttachment, error)
}

type CNIManager struct {
	backend           cniBackend
	ifName            string
	bridgeName        string
	bridgeNetworkName string
}

type CNIResult struct {
	IP  string
	DNS DNSConfig
}

func NewCNIManager(cfg CNIManagerConfig) (*CNIManager, error) {
	cfg = normalizeCNIManagerConfig(cfg)

	backend, err := newLibCNIBackend(cfg)
	if err != nil {
		return nil, err
	}
	bridgeNetworkName, bridgeName, err := loadedBridgeNetwork(backend.outerNetwork.config)
	if err != nil {
		return nil, fmt.Errorf("inspect loaded CNI config: %w", err)
	}

	return &CNIManager{
		backend:           backend,
		ifName:            cniOuterInterfaceName,
		bridgeName:        bridgeName,
		bridgeNetworkName: bridgeNetworkName,
	}, nil
}

func normalizeCNIManagerConfig(cfg CNIManagerConfig) CNIManagerConfig {
	if len(cfg.PluginBinDirs) == 0 {
		cfg.PluginBinDirs = []string{defaultCNIPluginBinDir}
	}
	if cfg.PluginConfDir == "" {
		cfg.PluginConfDir = defaultCNIPluginConfDir
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = defaultCNICacheDir
	}
	return cfg
}

func extractCNIIP(result *types100.Result) (string, error) {
	if result == nil {
		return "", fmt.Errorf("cni returned nil result")
	}
	for _, ipConfig := range result.IPs {
		if ipConfig == nil || ipConfig.Interface == nil || ipConfig.Address.IP.To4() == nil {
			continue
		}
		index := *ipConfig.Interface
		if index < 0 || index >= len(result.Interfaces) || result.Interfaces[index] == nil {
			return "", fmt.Errorf("cni returned IP with invalid interface index %d", index)
		}
		if result.Interfaces[index].Name == cniOuterInterfaceName {
			return ipConfig.Address.IP.To4().String(), nil
		}
	}
	return "", fmt.Errorf("cni returned no IPv4 address for interface %q", cniOuterInterfaceName)
}

func extractCNIDNS(result *types100.Result) (DNSConfig, error) {
	if result == nil {
		return DNSConfig{}, fmt.Errorf("cni returned nil result")
	}
	return NormalizeDNS(DNSConfig{
		Nameservers: result.DNS.Nameservers,
		Domain:      result.DNS.Domain,
		Search:      result.DNS.Search,
		Options:     result.DNS.Options,
	})
}

// SetupSandboxNetwork performs CNI ADD and extracts the sandbox network result. The caller
// owns rollback on every error because ADD may have taken effect before failing.
func (m *CNIManager) SetupSandboxNetwork(ctx context.Context, cniID string, netnsPath string) (CNIResult, error) {
	if m == nil || m.backend == nil {
		return CNIResult{}, fmt.Errorf("cni config not initialized")
	}
	result, err := m.backend.Setup(ctx, cniID, netnsPath)
	if err != nil {
		return CNIResult{}, fmt.Errorf("failed to setup cni network: %w", err)
	}
	cniIP, err := extractCNIIP(result)
	if err != nil {
		return CNIResult{}, fmt.Errorf("failed to extract cni IP: %w", err)
	}
	dns, err := extractCNIDNS(result)
	if err != nil {
		return CNIResult{}, fmt.Errorf("failed to extract cni DNS: %w", err)
	}
	return CNIResult{IP: cniIP, DNS: dns}, nil
}

func (m *CNIManager) TeardownSandboxNetwork(ctx context.Context, cniID string, netnsPath string) error {
	if cniID == "" {
		return nil
	}
	if m == nil || m.backend == nil {
		return fmt.Errorf("cni config not initialized")
	}
	return m.backend.Remove(ctx, cniID, netnsPath)
}

func (m *CNIManager) checkSandboxInterface(ctx context.Context, netnsPath string, cniIP string) error {
	if m == nil {
		return fmt.Errorf("cni config not initialized")
	}
	ifName := m.ifName
	if ifName == "" {
		ifName = cniOuterInterfaceName
	}
	return runInNetNSPath(ctx, netnsPath, func() error {
		cniLink, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("cni interface %s missing: %w", ifName, err)
		}
		expectedIP := parseCNIIP(cniIP)
		if expectedIP == nil {
			return fmt.Errorf("stored cni IP %q is invalid", cniIP)
		}
		hasIP, err := linkHasIP(cniLink, expectedIP)
		if err != nil {
			return fmt.Errorf("checking cni interface %s addresses: %w", ifName, err)
		}
		if !hasIP {
			return fmt.Errorf("cni interface %s missing stored IP %s", ifName, expectedIP.String())
		}
		return nil
	})
}

func parseCNIIP(raw string) net.IP {
	if ip := net.ParseIP(raw); ip != nil {
		return ip
	}
	ip, _, err := net.ParseCIDR(raw)
	if err != nil {
		return nil
	}
	return ip
}

func linkHasIP(link netlink.Link, ip net.IP) (bool, error) {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return false, err
	}
	for _, addr := range addrs {
		if addr.IP.Equal(ip) {
			return true, nil
		}
	}
	return false, nil
}
