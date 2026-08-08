package netstack

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	cni "github.com/containerd/go-cni"
	"github.com/vishvananda/netlink"
)

const (
	DefaultCNIIfName           = "eth0"
	DefaultCNIPluginConfDir    = "/etc/conch/cni/net.d"
	DefaultCNIPluginBinDir     = "/usr/libexec/cni"
	defaultCNIPluginMaxConfNum = 1
	defaultCNIMinNetworkCount  = defaultCNIPluginMaxConfNum + 1 // loopback plus the outer sandbox network
	defaultCNIIfName           = DefaultCNIIfName
	defaultCNIInterfacePrefix  = "eth"
	defaultCNIPluginConfDir    = DefaultCNIPluginConfDir
	defaultCNIPluginBinDir     = DefaultCNIPluginBinDir
	cniTeardownRetryAttempts   = 3
	cniTeardownRetryDelay      = 100 * time.Millisecond
)

type CNIManagerConfig struct {
	PluginBinDirs []string `toml:"plugin_bin_dirs" json:"pluginBinDirs" yaml:"plugin_bin_dirs"`
	PluginConfDir string   `toml:"plugin_conf_dir" json:"pluginConfDir" yaml:"plugin_conf_dir"`
	IfName        string   `toml:"if_name" json:"ifName" yaml:"if_name"`
}

type cniPlugin interface {
	Setup(context.Context, string, string, ...cni.NamespaceOpts) (*cni.Result, error)
	Remove(context.Context, string, string, ...cni.NamespaceOpts) error
	GetConfig() *cni.ConfigResult
}

type CNIManager struct {
	plugin     cniPlugin
	config     CNIManagerConfig
	bridgeName string
}

func NewCNIManager(cfg CNIManagerConfig) (*CNIManager, error) {
	cfg = normalizeCNIManagerConfig(cfg)
	ifPrefix := strings.TrimRight(cfg.IfName, "0123456789")
	if ifPrefix == "" {
		ifPrefix = defaultCNIInterfacePrefix
	}
	if generated := fmt.Sprintf("%s0", ifPrefix); generated != cfg.IfName {
		return nil, fmt.Errorf("if_name %q is incompatible with go-cni interface prefix %q; expected %q", cfg.IfName, ifPrefix, generated)
	}
	plugin, err := cni.New(
		cni.WithMinNetworkCount(defaultCNIMinNetworkCount),
		cni.WithPluginConfDir(cfg.PluginConfDir),
		cni.WithPluginMaxConfNum(defaultCNIPluginMaxConfNum),
		cni.WithPluginDir(cfg.PluginBinDirs),
		cni.WithInterfacePrefix(ifPrefix),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize cni: %w", err)
	}
	if err := plugin.Load(cni.WithLoNetwork, cni.WithDefaultConf); err != nil {
		return nil, fmt.Errorf("failed to load cni config: %w", err)
	}
	bridgeName, err := loadedBridgeName(plugin.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("inspect loaded cni config: %w", err)
	}

	return &CNIManager{
		plugin:     plugin,
		config:     cfg,
		bridgeName: bridgeName,
	}, nil
}

type bridgePluginConfig struct {
	Bridge string `json:"bridge"`
}

func loadedBridgeName(config *cni.ConfigResult) (string, error) {
	if config == nil {
		return "", fmt.Errorf("CNI returned no loaded configuration")
	}
	for _, network := range config.Networks {
		if network == nil || network.Config == nil || network.Config.Name == "cni-loopback" {
			continue
		}
		for _, plugin := range network.Config.Plugins {
			if plugin == nil || plugin.Network == nil || plugin.Network.Type != "bridge" {
				continue
			}
			var bridge bridgePluginConfig
			if err := json.Unmarshal([]byte(plugin.Source), &bridge); err != nil {
				return "", fmt.Errorf("parse bridge plugin config: %w", err)
			}
			if strings.TrimSpace(bridge.Bridge) == "" {
				return "", fmt.Errorf("loaded bridge network %q has no bridge name", network.Config.Name)
			}
			return bridge.Bridge, nil
		}
	}
	return "", fmt.Errorf("loaded CNI configuration has no bridge network")
}

func normalizeCNIManagerConfig(cfg CNIManagerConfig) CNIManagerConfig {
	if len(cfg.PluginBinDirs) == 0 {
		cfg.PluginBinDirs = []string{defaultCNIPluginBinDir}
	}
	if cfg.PluginConfDir == "" {
		cfg.PluginConfDir = defaultCNIPluginConfDir
	}
	if cfg.IfName == "" {
		cfg.IfName = defaultCNIIfName
	}
	return cfg
}

func extractCNIIP(result *cni.Result, defaultIfName string) (string, error) {
	if result == nil {
		return "", fmt.Errorf("cni returned nil result")
	}
	if defaultIfName == "" {
		defaultIfName = defaultCNIIfName
	}

	defaultIface := result.Interfaces[defaultIfName]
	if defaultIface == nil || len(defaultIface.IPConfigs) == 0 {
		for _, iface := range result.Interfaces {
			if iface != nil && len(iface.IPConfigs) > 0 {
				defaultIface = iface
				break
			}
		}
	}
	if defaultIface == nil || len(defaultIface.IPConfigs) == 0 {
		return "", fmt.Errorf("failed to find network info for sandbox interface %q", defaultIfName)
	}

	for _, ipConfig := range defaultIface.IPConfigs {
		if ipConfig == nil || ipConfig.IP == nil {
			continue
		}
		if ipConfig.IP.To4() != nil {
			return ipConfig.IP.String(), nil
		}
	}
	for _, ipConfig := range defaultIface.IPConfigs {
		if ipConfig != nil && ipConfig.IP != nil {
			return ipConfig.IP.String(), nil
		}
	}
	return "", fmt.Errorf("failed to find IP for sandbox interface %q", defaultIfName)
}

// SetupSandboxNetwork performs CNI ADD and extracts the sandbox IP. The caller
// owns rollback on every error because ADD may have taken effect before failing.
func (m *CNIManager) SetupSandboxNetwork(ctx context.Context, cniID string, netnsPath string) (string, error) {
	if m == nil || m.plugin == nil {
		return "", fmt.Errorf("cni config not initialized")
	}
	result, err := m.plugin.Setup(ctx, cniID, netnsPath)
	if err != nil {
		return "", fmt.Errorf("failed to setup cni network: %w", err)
	}
	cniIP, err := extractCNIIP(result, m.config.IfName)
	if err != nil {
		return "", fmt.Errorf("failed to extract cni IP: %w", err)
	}
	return cniIP, nil
}

func (m *CNIManager) TeardownSandboxNetwork(ctx context.Context, cniID string, netnsPath string) error {
	if cniID == "" {
		return nil
	}
	if m == nil || m.plugin == nil {
		return fmt.Errorf("cni config not initialized")
	}
	return m.plugin.Remove(ctx, cniID, netnsPath)
}

func (m *CNIManager) checkSandboxInterface(ctx context.Context, netnsPath string, cniIP string) error {
	if m == nil {
		return fmt.Errorf("cni config not initialized")
	}
	ifName := m.config.IfName
	if ifName == "" {
		ifName = defaultCNIIfName
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
