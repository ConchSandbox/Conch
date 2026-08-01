package netstack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	cni "github.com/containerd/go-cni"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	DefaultCNIIfName            = "eth0"
	DefaultCNIPluginConfDir     = "/etc/conch/cni/net.d"
	DefaultCNIPluginBinDir      = "/usr/libexec/cni"
	DefaultCNIPluginMaxConf     = 1
	defaultCNIIfName            = DefaultCNIIfName
	defaultCNIInterfacePrefix   = "eth"
	defaultCNIPluginConfDir     = DefaultCNIPluginConfDir
	defaultCNIPluginBinDir      = DefaultCNIPluginBinDir
	defaultCNIPluginMaxConf     = DefaultCNIPluginMaxConf
	defaultCNINamespace         = "conch"
	defaultHostLocalIPAMDataDir = "/var/lib/cni/networks"
	cniTeardownRetryAttempts    = 3
	cniTeardownRetryDelay       = 100 * time.Millisecond
)

type NamespaceOpts = cni.NamespaceOpts

type CNIManagerConfig struct {
	PluginBinDirs   []string `toml:"plugin_bin_dirs" json:"pluginBinDirs" yaml:"plugin_bin_dirs"`
	PluginConfDir   string   `toml:"plugin_conf_dir" json:"pluginConfDir" yaml:"plugin_conf_dir"`
	PluginMaxConf   int      `toml:"plugin_max_conf" json:"pluginMaxConf" yaml:"plugin_max_conf"`
	IfName          string   `toml:"if_name" json:"ifName" yaml:"if_name"`
	SetupSerially   bool     `toml:"setup_serially" json:"setupSerially" yaml:"setup_serially"`
	MinNetworkCount int      `toml:"min_network_count" json:"minNetworkCount" yaml:"min_network_count"`
}

type CNIManager struct {
	plugin                    cni.CNI
	config                    CNIManagerConfig
	selectedConf              string
	selectedBridgeNames       []string
	selectedHostLocalAllocDir string
}

type cniConfigFile struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Bridge  string            `json:"bridge"`
	IPAM    cniIPAMConfig     `json:"ipam"`
	Plugins []cniPluginConfig `json:"plugins"`
}

type cniPluginConfig struct {
	Type   string        `json:"type"`
	Bridge string        `json:"bridge"`
	IPAM   cniIPAMConfig `json:"ipam"`
}

type cniIPAMConfig struct {
	Type    string `json:"type"`
	DataDir string `json:"dataDir"`
}

type CNIResult struct {
	IP            string
	AdditionalIPs []string
	Interfaces    []CNIInterface
	Routes        []CNIRoute
	DNS           []CNIDNS
}

type CNIInterface struct {
	Name      string
	Mac       string
	Sandbox   string
	IPConfigs []CNIIPConfig
}

type CNIIPConfig struct {
	IP      string
	Gateway string
}

type CNIRoute struct {
	Dst string
	GW  string
}

type CNIDNS struct {
	Nameservers []string
	Domain      string
	Search      []string
	Options     []string
}

type cniSelectedMetadata struct {
	bridgeNames       []string
	hostLocalAllocDir string
}

func NewCNIManager(cfg CNIManagerConfig) (*CNIManager, error) {
	cfg = normalizeCNIManagerConfig(cfg)
	ifPrefix := interfacePrefix(cfg.IfName)
	if generated := defaultIfNameForPrefix(ifPrefix); generated != cfg.IfName {
		return nil, fmt.Errorf("if_name %q is incompatible with go-cni interface prefix %q; expected %q", cfg.IfName, ifPrefix, generated)
	}
	plugin, err := cni.New(
		cni.WithMinNetworkCount(cfg.MinNetworkCount),
		cni.WithPluginConfDir(cfg.PluginConfDir),
		cni.WithPluginMaxConfNum(cfg.PluginMaxConf),
		cni.WithPluginDir(cfg.PluginBinDirs),
		cni.WithInterfacePrefix(ifPrefix),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize cni: %w", err)
	}
	if err := plugin.Load(cni.WithLoNetwork, cni.WithDefaultConf); err != nil {
		return nil, fmt.Errorf("failed to load cni config: %w", err)
	}

	manager := &CNIManager{
		plugin: plugin,
		config: cfg,
	}
	manager.selectedConf = selectedCNIConfigName(plugin)
	metadata, err := selectedCNIConfigMetadata(cfg.PluginConfDir, manager.selectedConf)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect selected cni bridge config: %w", err)
	}
	manager.selectedBridgeNames = metadata.bridgeNames
	manager.selectedHostLocalAllocDir = metadata.hostLocalAllocDir
	return manager, nil
}

func normalizeCNIManagerConfig(cfg CNIManagerConfig) CNIManagerConfig {
	if len(cfg.PluginBinDirs) == 0 {
		cfg.PluginBinDirs = []string{defaultCNIPluginBinDir}
	}
	if cfg.PluginConfDir == "" {
		cfg.PluginConfDir = defaultCNIPluginConfDir
	}
	if cfg.PluginMaxConf == 0 {
		cfg.PluginMaxConf = defaultCNIPluginMaxConf
	}
	if cfg.IfName == "" {
		cfg.IfName = defaultCNIIfName
	}
	if cfg.MinNetworkCount == 0 {
		// loopback plus the outer sandbox network
		cfg.MinNetworkCount = 2
	}
	return cfg
}

func interfacePrefix(ifName string) string {
	if ifName == "" {
		return defaultCNIInterfacePrefix
	}
	prefix := strings.TrimRight(ifName, "0123456789")
	if prefix == "" {
		return defaultCNIInterfacePrefix
	}
	return prefix
}

func defaultIfNameForPrefix(prefix string) string {
	if prefix == "" {
		prefix = defaultCNIInterfacePrefix
	}
	return fmt.Sprintf("%s0", prefix)
}

func selectedCNIConfigName(plugin cni.CNI) string {
	config := plugin.GetConfig()
	if config == nil || len(config.Networks) == 0 {
		return ""
	}
	for _, network := range config.Networks {
		if network == nil || network.Config == nil {
			continue
		}
		name := network.Config.Name
		switch strings.ToLower(name) {
		case "", "lo", "loopback", "cni-loopback":
			continue
		default:
			return name
		}
	}
	return ""
}

func (m *CNIManager) SelectCNIPluginAndConfig(slot *Slot) (cni.CNI, string, error) {
	if m == nil || m.plugin == nil {
		return nil, "", fmt.Errorf("cni config not initialized")
	}
	if slot == nil {
		return nil, "", fmt.Errorf("slot is nil")
	}
	// Bridge sharding remains Conch policy. A deployment can map each shard to
	// a CNI config directory before constructing this manager; this exposes the
	// selected config name for callers that need to record that policy decision.
	return m.plugin, m.selectedConf, nil
}

func (m *CNIManager) SelectedBridgeNames() ([]string, error) {
	if m == nil {
		return nil, nil
	}
	return append([]string(nil), m.selectedBridgeNames...), nil
}

func selectedCNIConfigMetadata(confDir, selectedConf string) (cniSelectedMetadata, error) {
	var metadata cniSelectedMetadata
	if confDir == "" || selectedConf == "" {
		return metadata, nil
	}
	entries, err := os.ReadDir(confDir)
	if err != nil {
		return metadata, fmt.Errorf("reading cni config dir %s: %w", confDir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".conf" && ext != ".conflist" {
			continue
		}
		path := filepath.Join(confDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			getLogger().Warn("failed to read unrelated cni config while inspecting selected config", ulog.F("path", path), ulog.F("error", err))
			continue
		}
		var named struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &named); err != nil {
			getLogger().Warn("failed to parse unrelated cni config while inspecting selected config", ulog.F("path", path), ulog.F("error", err))
			continue
		}
		if named.Name != selectedConf {
			continue
		}
		var cfg cniConfigFile
		if err := json.Unmarshal(data, &cfg); err != nil {
			return metadata, fmt.Errorf("parsing selected cni config %s: %w", path, err)
		}
		metadata.bridgeNames = cfg.bridgeNames()
		metadata.hostLocalAllocDir = cfg.hostLocalAllocDir()
		return metadata, nil
	}
	getLogger().Warn("selected cni config was not found while inspecting cleanup metadata", ulog.F("config", selectedConf), ulog.F("dir", confDir))
	return metadata, nil
}

func buildCNIOpts(slot *Slot, cniID, netnsPath string) ([]NamespaceOpts, error) {
	if slot == nil {
		return nil, fmt.Errorf("slot is nil")
	}
	if cniID == "" {
		return nil, fmt.Errorf("cniID is required")
	}
	if netnsPath == "" {
		return nil, fmt.Errorf("netnsPath is required")
	}
	return []NamespaceOpts{
		cni.WithLabels(map[string]string{
			"K8S_POD_NAMESPACE":          defaultCNINamespace,
			"K8S_POD_NAME":               cniID,
			"K8S_POD_INFRA_CONTAINER_ID": cniID,
			"CONCH_NETWORK_SLOT":         slot.Key,
			"IgnoreUnknown":              "1",
		}),
	}, nil
}

func (c cniConfigFile) bridgeNames() []string {
	seen := make(map[string]struct{})
	var bridges []string
	appendBridge := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		bridges = append(bridges, name)
	}
	if strings.EqualFold(c.Type, "bridge") {
		if c.Bridge != "" {
			appendBridge(c.Bridge)
		} else {
			getLogger().Warn("cni bridge plugin requires an explicit bridge value for Conch cleanup")
		}
	}
	for _, plugin := range c.Plugins {
		if strings.EqualFold(plugin.Type, "bridge") {
			if plugin.Bridge != "" {
				appendBridge(plugin.Bridge)
			} else {
				getLogger().Warn("cni bridge plugin requires an explicit bridge value for Conch cleanup")
			}
		}
	}
	return bridges
}

func (c cniConfigFile) hostLocalAllocDir() string {
	if strings.EqualFold(c.IPAM.Type, "host-local") {
		return filepath.Join(c.IPAM.dataDir(), c.Name)
	}
	for _, plugin := range c.Plugins {
		if strings.EqualFold(plugin.IPAM.Type, "host-local") {
			return filepath.Join(plugin.IPAM.dataDir(), c.Name)
		}
	}
	return ""
}

func (i cniIPAMConfig) dataDir() string {
	if i.DataDir != "" {
		return i.DataDir
	}
	return defaultHostLocalIPAMDataDir
}

func convertCNIResult(result *cni.Result, defaultIfName string) (*CNIResult, error) {
	if result == nil {
		return nil, fmt.Errorf("cni returned nil result")
	}
	if defaultIfName == "" {
		defaultIfName = defaultCNIIfName
	}

	out := &CNIResult{
		Interfaces: make([]CNIInterface, 0, len(result.Interfaces)),
		Routes:     make([]CNIRoute, 0, len(result.Routes)),
		DNS:        make([]CNIDNS, 0, len(result.DNS)),
	}

	for name, iface := range result.Interfaces {
		if iface == nil {
			continue
		}
		converted := CNIInterface{
			Name:      name,
			Mac:       iface.Mac,
			Sandbox:   iface.Sandbox,
			IPConfigs: make([]CNIIPConfig, 0, len(iface.IPConfigs)),
		}
		for _, ipConfig := range iface.IPConfigs {
			if ipConfig == nil {
				continue
			}
			converted.IPConfigs = append(converted.IPConfigs, CNIIPConfig{
				IP:      ipConfig.IP.String(),
				Gateway: ipToString(ipConfig.Gateway),
			})
		}
		out.Interfaces = append(out.Interfaces, converted)
	}

	for _, route := range result.Routes {
		if route == nil {
			continue
		}
		out.Routes = append(out.Routes, CNIRoute{
			Dst: route.Dst.String(),
			GW:  ipToString(route.GW),
		})
	}
	for _, dns := range result.DNS {
		cniDNS := CNIDNS{
			Nameservers: dns.Nameservers,
			Domain:      dns.Domain,
			Search:      dns.Search,
			Options:     dns.Options,
		}
		out.DNS = append(out.DNS, cniDNS)
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
		return nil, fmt.Errorf("failed to find network info for sandbox interface %q", defaultIfName)
	}

	for _, ipConfig := range defaultIface.IPConfigs {
		if ipConfig == nil {
			continue
		}
		ip := ipConfig.IP.String()
		if out.IP == "" && ipConfig.IP.To4() != nil {
			out.IP = ip
			continue
		}
		out.AdditionalIPs = append(out.AdditionalIPs, ip)
	}
	if out.IP == "" {
		out.IP = defaultIface.IPConfigs[0].IP.String()
		if len(out.AdditionalIPs) > 0 && out.AdditionalIPs[0] == out.IP {
			out.AdditionalIPs = out.AdditionalIPs[1:]
		}
	}

	return out, nil
}

func ipToString(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}

func (m *CNIManager) SetupSandboxNetwork(ctx context.Context, cniID string, netnsPath string, opts ...NamespaceOpts) (*CNIResult, error) {
	if m == nil || m.plugin == nil {
		return nil, fmt.Errorf("cni config not initialized")
	}
	var (
		result *cni.Result
		err    error
	)
	if m.config.SetupSerially {
		result, err = m.plugin.SetupSerially(ctx, cniID, netnsPath, opts...)
	} else {
		result, err = m.plugin.Setup(ctx, cniID, netnsPath, opts...)
	}
	if err != nil {
		if isExpectedShutdownError(ctx, err) {
			return nil, errors.Join(err, ctx.Err())
		}
		return nil, m.rollbackCNISetup(ctx, cniID, netnsPath, fmt.Errorf("failed to setup cni network: %w", err), opts...)
	}
	converted, err := convertCNIResult(result, m.config.IfName)
	if err != nil {
		if shouldPreserveAfterCancel(ctx) {
			return nil, errors.Join(err, ctx.Err())
		}
		return nil, m.rollbackCNISetup(ctx, cniID, netnsPath, fmt.Errorf("failed to convert cni result: %w", err), opts...)
	}
	return converted, nil
}

func (m *CNIManager) rollbackCNISetup(ctx context.Context, cniID, netnsPath string, cause error, opts ...NamespaceOpts) error {
	removeErr := m.plugin.Remove(context.WithoutCancel(ctx), cniID, netnsPath, opts...)
	allocErr := m.validateHostLocalAllocationReleased(cniID)
	if removeErr != nil {
		return errors.Join(cause, fmt.Errorf("failed to rollback cni setup: %w", removeErr), allocErr)
	}
	if allocErr != nil {
		return errors.Join(cause, fmt.Errorf("cni rollback left host-local allocation: %w", allocErr))
	}
	return cause
}

func (m *CNIManager) validateHostLocalAllocationReleased(cniID string) error {
	if m == nil || m.selectedHostLocalAllocDir == "" || cniID == "" {
		return nil
	}
	entries, err := os.ReadDir(m.selectedHostLocalAllocDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading host-local allocation dir %s: %w", m.selectedHostLocalAllocDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "last_reserved_ip") || entry.Name() == "lock" {
			continue
		}
		path := filepath.Join(m.selectedHostLocalAllocDir, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading host-local allocation %s: %w", path, err)
		}
		if strings.Contains(string(content), cniID) {
			return fmt.Errorf("host-local allocation %s still references cni id %s", path, cniID)
		}
	}
	return nil
}

func (m *CNIManager) TeardownSandboxNetwork(ctx context.Context, cniID string, netnsPath string, opts ...NamespaceOpts) error {
	if cniID == "" || netnsPath == "" {
		return nil
	}
	if m == nil || m.plugin == nil {
		return fmt.Errorf("cni config not initialized")
	}
	return m.plugin.Remove(ctx, cniID, netnsPath, opts...)
}
