package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/pkg/ulog"
	"gopkg.in/yaml.v3"
)

var WorkDir = defaultWorkDir

const defaultWorkDir = "/var/run/conch"

// Config holds the application configuration
type Config struct {
	App        AppConfig        `yaml:"app"`
	Log        LogConfig        `yaml:"log"`
	Server     ServerConfig     `yaml:"server"`
	Network    NetworkConfig    `yaml:"network"`
	Containerd ContainerdConfig `yaml:"containerd"`
	Image      ImageConfig      `yaml:"image"`
	Sandbox    SandboxConfig    `yaml:"sandbox"`
	Volume     VolumeConfig     `yaml:"volume"`
	State      StateConfig      `yaml:"state"`
	CRI        CRIConfig        `yaml:"cri"`
}

// AppConfig holds application-specific configuration
type AppConfig struct {
	Name string `yaml:"name"`
}

// LogConfig holds logging configuration
type LogConfig struct {
	Level  string `yaml:"level"`
	Output string `yaml:"output"` // "stdout", "file", or "both"
}

// ServerConfig holds server configuration
type ServerConfig struct {
	Host       string  `yaml:"host"`
	Port       int     `yaml:"port"`
	UnixSocket *string `yaml:"unix_socket"`
	PIDFile    string  `yaml:"pid_file"`
	WorkDir    string  `yaml:"work_dir"`
}

// NetworkConfig holds network pool configuration
type NetworkConfig struct {
	PoolSize           int       `yaml:"pool_size"`
	DynamicReservation bool      `yaml:"dynamic_reservation"`
	BridgeCount        int       `yaml:"bridge_count"`
	TapIP              string    `yaml:"tap_ip"`
	TapMask            int       `yaml:"tap_mask"`
	CNI                CNIConfig `yaml:"cni"`
}

// CNIConfig holds the plugin directories and runtime behavior for outer sandbox networking.
type CNIConfig = netstack.CNIManagerConfig

// ContainerdConfig holds containerd runtime configuration
type ContainerdConfig struct {
	RootDir          string `yaml:"root_dir"`
	StateDir         string `yaml:"state_dir"`
	DefaultNamespace string `yaml:"default_namespace"`
}

// ImageConfig holds image workflow defaults.
type ImageConfig struct {
	DefaultKernelImage            string `yaml:"default_kernel_image"`
	DefaultKernelPlainHTTP        bool   `yaml:"default_kernel_plain_http"`
	DefaultKernelRegistryUsername string `yaml:"default_kernel_registry_username"`
	DefaultKernelRegistryPassword string `yaml:"default_kernel_registry_password"`
}

const (
	DefaultKernelImage = "hub.oepkgs.net/conch/kernel:6.6.0"
	DefaultVMMName     = "stratovirt"
)

type SandboxConfig struct {
	VsockSignalRetry   time.Duration `yaml:"vsock_signal_retry"`
	VsockSignalTimeout time.Duration `yaml:"vsock_signal_timeout"`
	RequestTimeout     time.Duration `yaml:"request_timeout"`
	DefaultTemplateID  string        `yaml:"default_template_id"`
	DefaultVMMName     string        `yaml:"default_vmm_name"`
	DefaultVCPUNum     int64         `yaml:"default_vcpu_num"`
	DefaultVCPUMax     int64         `yaml:"default_vcpu_max"`
	DefaultRAMMB       int64         `yaml:"default_ram_mb"`
}

type VolumeConfig struct {
	MaxMounts int                  `yaml:"max_mounts"`
	Backend   string               `yaml:"backend"`
	Virtiofs  VolumeVirtiofsConfig `yaml:"virtiofs"`
}

type VolumeVirtiofsConfig struct {
	Binary     string `yaml:"binary"`
	RuntimeDir string `yaml:"runtime_dir"`
}

type StateConfig struct {
	Path string `yaml:"path"`
}

type CRIConfig struct {
	Enabled bool   `yaml:"enabled"`
	Socket  string `yaml:"socket"`
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	defaultUnixSocket := "/var/run/conchd/conchd.sock"
	defaultPIDFile := "/var/run/conchd/conchd.pid"
	defaultWorkDir := "/var/run/conch"
	return &Config{
		App: AppConfig{
			Name: "conch",
		},
		Log: LogConfig{
			Level:  "debug",
			Output: "stdout",
		},
		Server: ServerConfig{
			Host:       "127.0.0.1",
			Port:       4063,
			UnixSocket: &defaultUnixSocket,
			PIDFile:    defaultPIDFile,
			WorkDir:    defaultWorkDir,
		},
		Network: NetworkConfig{
			PoolSize:           250,
			DynamicReservation: false,
			BridgeCount:        1,
			TapIP:              "192.168.100.2",
			TapMask:            24,
			CNI: CNIConfig{
				PluginBinDirs: []string{netstack.DefaultCNIPluginBinDir},
				PluginConfDir: netstack.DefaultCNIPluginConfDir,
				PluginMaxConf: netstack.DefaultCNIPluginMaxConf,
				IfName:        netstack.DefaultCNIIfName,
			},
		},
		Containerd: ContainerdConfig{
			RootDir:          "/var/lib/conch/containerd",
			StateDir:         "/run/conch/containerd",
			DefaultNamespace: "default",
		},
		Image: ImageConfig{
			DefaultKernelImage:            DefaultKernelImage,
			DefaultKernelPlainHTTP:        false,
			DefaultKernelRegistryUsername: "",
			DefaultKernelRegistryPassword: "",
		},
		Sandbox: SandboxConfig{
			VsockSignalRetry:   10 * time.Millisecond,
			VsockSignalTimeout: 60 * time.Second,
			RequestTimeout:     60 * time.Second,
			DefaultTemplateID:  "",
			DefaultVMMName:     DefaultVMMName,
			DefaultVCPUNum:     2,
			DefaultVCPUMax:     2,
			DefaultRAMMB:       4096,
		},
		Volume: VolumeConfig{
			MaxMounts: 10,
			Backend:   "virtiofs",
			Virtiofs: VolumeVirtiofsConfig{
				Binary:     "virtiofsd",
				RuntimeDir: "/run/conch/sandboxes",
			},
		},
		State: StateConfig{
			Path: "/var/lib/conch/state.db",
		},
		CRI: CRIConfig{
			Enabled: false,
			Socket:  "/var/run/conchd/conch-cri.sock",
		},
	}
}

// LoadConfig loads configuration from the specified file path
func LoadConfig(configPath string) (*Config, error) {
	// If config path is empty, use default config
	if configPath == "" {
		return DefaultConfig(), nil
	}
	if absPath, err := filepath.Abs(configPath); err == nil {
		configPath = absPath
	}

	// Read config file
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Config file doesn't exist, use default
			return DefaultConfig(), nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Parse YAML
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Merge with defaults to ensure all fields are populated
	defaultCfg := DefaultConfig()
	if cfg.App.Name == "" {
		cfg.App.Name = defaultCfg.App.Name
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = defaultCfg.Log.Level
	}
	if cfg.Log.Output == "" {
		cfg.Log.Output = defaultCfg.Log.Output
	}
	if cfg.Server.Host == "" {
		cfg.Server.Host = defaultCfg.Server.Host
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = defaultCfg.Server.Port
	}
	if cfg.Server.UnixSocket == nil {
		cfg.Server.UnixSocket = defaultCfg.Server.UnixSocket
	}
	if cfg.Server.PIDFile == "" {
		cfg.Server.PIDFile = defaultCfg.Server.PIDFile
	}
	if cfg.Server.WorkDir == "" {
		cfg.Server.WorkDir = defaultCfg.Server.WorkDir
	}
	if cfg.Network.PoolSize == 0 {
		cfg.Network.PoolSize = defaultCfg.Network.PoolSize
	}
	if cfg.Network.BridgeCount == 0 {
		cfg.Network.BridgeCount = defaultCfg.Network.BridgeCount
	}
	if cfg.Network.TapIP == "" {
		cfg.Network.TapIP = defaultCfg.Network.TapIP
	}
	if cfg.Network.TapMask == 0 {
		cfg.Network.TapMask = defaultCfg.Network.TapMask
	}
	if len(cfg.Network.CNI.PluginBinDirs) == 0 {
		cfg.Network.CNI.PluginBinDirs = defaultCfg.Network.CNI.PluginBinDirs
	}
	if cfg.Network.CNI.PluginConfDir == "" {
		cfg.Network.CNI.PluginConfDir = defaultCfg.Network.CNI.PluginConfDir
	}
	cfg.Network.CNI.PluginConfDir = resolveCNIPluginConfDir(configPath, cfg.Network.CNI.PluginConfDir, defaultCfg.Network.CNI.PluginConfDir)
	if cfg.Network.CNI.PluginMaxConf == 0 {
		cfg.Network.CNI.PluginMaxConf = defaultCfg.Network.CNI.PluginMaxConf
	}
	if cfg.Network.CNI.IfName == "" {
		cfg.Network.CNI.IfName = defaultCfg.Network.CNI.IfName
	}
	if cfg.Containerd.RootDir == "" {
		cfg.Containerd.RootDir = defaultCfg.Containerd.RootDir
	}
	if cfg.Containerd.StateDir == "" {
		cfg.Containerd.StateDir = defaultCfg.Containerd.StateDir
	}
	if cfg.Containerd.DefaultNamespace == "" {
		cfg.Containerd.DefaultNamespace = defaultCfg.Containerd.DefaultNamespace
	}
	if cfg.Image.DefaultKernelImage == "" {
		cfg.Image.DefaultKernelImage = defaultCfg.Image.DefaultKernelImage
	}
	if cfg.Sandbox.VsockSignalRetry == 0 {
		cfg.Sandbox.VsockSignalRetry = defaultCfg.Sandbox.VsockSignalRetry
	}
	if cfg.Sandbox.VsockSignalTimeout == 0 {
		cfg.Sandbox.VsockSignalTimeout = defaultCfg.Sandbox.VsockSignalTimeout
	}
	if cfg.Sandbox.RequestTimeout == 0 {
		cfg.Sandbox.RequestTimeout = defaultCfg.Sandbox.RequestTimeout
	}
	if cfg.Sandbox.DefaultTemplateID == "" {
		cfg.Sandbox.DefaultTemplateID = defaultCfg.Sandbox.DefaultTemplateID
	}
	if cfg.Sandbox.DefaultVMMName == "" {
		cfg.Sandbox.DefaultVMMName = defaultCfg.Sandbox.DefaultVMMName
	}
	if cfg.Sandbox.DefaultVCPUNum == 0 {
		cfg.Sandbox.DefaultVCPUNum = defaultCfg.Sandbox.DefaultVCPUNum
	}
	if cfg.Sandbox.DefaultVCPUMax == 0 {
		cfg.Sandbox.DefaultVCPUMax = defaultCfg.Sandbox.DefaultVCPUMax
	}
	if cfg.Sandbox.DefaultRAMMB == 0 {
		cfg.Sandbox.DefaultRAMMB = defaultCfg.Sandbox.DefaultRAMMB
	}
	if cfg.Volume.MaxMounts == 0 {
		cfg.Volume.MaxMounts = defaultCfg.Volume.MaxMounts
	}
	if cfg.Volume.Backend == "" {
		cfg.Volume.Backend = defaultCfg.Volume.Backend
	}
	if cfg.Volume.Virtiofs.Binary == "" {
		cfg.Volume.Virtiofs.Binary = defaultCfg.Volume.Virtiofs.Binary
	}
	if cfg.Volume.Virtiofs.RuntimeDir == "" {
		cfg.Volume.Virtiofs.RuntimeDir = defaultCfg.Volume.Virtiofs.RuntimeDir
	}
	if cfg.State.Path == "" {
		cfg.State.Path = defaultCfg.State.Path
	}
	if cfg.CRI.Socket == "" {
		cfg.CRI.Socket = defaultCfg.CRI.Socket
	}

	if cfg.Server.WorkDir != "" {
		WorkDir = cfg.Server.WorkDir
	}

	return &cfg, nil
}

func resolveCNIPluginConfDir(configPath, confDir, defaultConfDir string) string {
	if confDir != defaultConfDir || hasCNIConfig(confDir) {
		return confDir
	}
	localConfDir := filepath.Join(filepath.Dir(configPath), "cni", "net.d")
	if hasCNIConfig(localConfDir) {
		return localConfDir
	}
	return confDir
}

func hasCNIConfig(confDir string) bool {
	entries, err := os.ReadDir(confDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext == ".conf" || ext == ".conflist" {
			return true
		}
	}
	return false
}

// GetLogConfig converts LogConfig to ulog.Config
func (c *Config) GetLogConfig() (ulog.Config, error) {
	// Parse log level
	level, err := parseLogLevel(c.Log.Level)
	if err != nil {
		return ulog.Config{}, fmt.Errorf("invalid log level: %w", err)
	}

	// Determine output mode
	var stdout bool
	var outputPath string
	switch c.Log.Output {
	case "stdout":
		stdout = true
		outputPath = "" // No file output
	case "file":
		stdout = false
		outputPath = "/var/log/conchd/"
	case "both":
		stdout = true
		outputPath = "/var/log/conchd/"
	default:
		return ulog.Config{}, fmt.Errorf("invalid log output mode: %s (must be stdout, file, or both)", c.Log.Output)
	}

	return ulog.Config{
		Level:      level,
		OutputPath: outputPath,
		Stdout:     stdout,
	}, nil
}

// GetServerAddress returns the server address in "host:port" format
func (c *Config) GetServerAddress() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}

// GetServerUnixSocket returns the configured unix socket path when explicitly set.
func (c *Config) GetServerUnixSocket() string {
	if c == nil || c.Server.UnixSocket == nil {
		return ""
	}
	return *c.Server.UnixSocket
}

// parseLogLevel converts string log level to ulog.LogLevel
func parseLogLevel(level string) (ulog.LogLevel, error) {
	switch level {
	case "debug":
		return ulog.DebugLevel, nil
	case "info":
		return ulog.InfoLevel, nil
	case "warn":
		return ulog.WarnLevel, nil
	case "error":
		return ulog.ErrorLevel, nil
	case "fatal":
		return ulog.FatalLevel, nil
	default:
		return 0, fmt.Errorf("unknown log level: %s", level)
	}
}

// FindConfigFile tries to find the config file in common locations
func FindConfigFile() string {
	// Check common config file locations
	locations := []string{
		"/etc/conch/config.yaml",
		"config/config.yaml",
	}

	for _, loc := range locations {
		if _, err := os.Stat(loc); err == nil {
			absPath, _ := filepath.Abs(loc)
			return absPath
		}
	}

	return ""
}
