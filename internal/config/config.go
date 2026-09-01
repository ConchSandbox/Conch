package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/pkg/ulog"
	"gopkg.in/yaml.v3"
)

var WorkDir = defaultWorkDir

const (
	defaultWorkDir  = "/var/run/conch"
	defaultStateDir = "/var/lib/conch"
)

// Config holds the application configuration
type Config struct {
	App     AppConfig     `yaml:"app"`
	Log     LogConfig     `yaml:"log"`
	Server  ServerConfig  `yaml:"server"`
	Network NetworkConfig `yaml:"network"`
	Sandbox SandboxConfig `yaml:"sandbox"`
	Volume  VolumeConfig  `yaml:"volume"`
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

// ServerConfig holds server configuration. conchd only ever serves the API on
// a local Unix socket; there is no TCP listener.
type ServerConfig struct {
	WorkDir  string `yaml:"work_dir"`
	StateDir string `yaml:"state_dir"`
}

// NetworkConfig holds network pool configuration
type NetworkConfig struct {
	WarmPoolSize int       `yaml:"warm_pool_size"`
	CNI          CNIConfig `yaml:"cni"`
}

// CNIConfig holds the plugin directories and runtime behavior for outer sandbox networking.
type CNIConfig = netstack.CNIManagerConfig

type VMMBinaryConfig struct {
	Binary string `yaml:"binary"`
}

const (
	DefaultSandboxBackend = "stratovirt"
	defaultVolumeBackend  = "virtiofs"
)

type SandboxConfig struct {
	VsockSignalRetry   time.Duration    `yaml:"vsock_signal_retry"`
	VsockSignalTimeout time.Duration    `yaml:"vsock_signal_timeout"`
	RequestTimeout     time.Duration    `yaml:"request_timeout"`
	Backend            string           `yaml:"backend"`
	DefaultSpec        SandboxSpec      `yaml:"default_spec"`
	CloudHypervisor    *VMMBinaryConfig `yaml:"cloud_hypervisor"`
	Stratovirt         *VMMBinaryConfig `yaml:"stratovirt"`
}

type SandboxSpec struct {
	TemplateName string `yaml:"template_name"`
	TemplateID   string `yaml:"template_id"`
	VCPUNum      int64  `yaml:"vcpu_num"`
	VCPUMax      int64  `yaml:"vcpu_max"`
	RamMB        int64  `yaml:"ram_mb"`
}

// BinaryPaths returns the explicitly configured binary for each available VMM.
func (c SandboxConfig) BinaryPaths() map[string]string {
	paths := make(map[string]string, 2)
	if c.CloudHypervisor != nil {
		paths["cloud-hypervisor"] = c.CloudHypervisor.Binary
	}
	if c.Stratovirt != nil {
		paths["stratovirt"] = c.Stratovirt.Binary
	}
	return paths
}

type VolumeConfig struct {
	MaxMounts int                  `yaml:"max_mounts"`
	Backend   string               `yaml:"backend"`
	Virtiofs  VolumeVirtiofsConfig `yaml:"virtiofs"`
}

type VolumeVirtiofsConfig struct {
	Binary string `yaml:"binary"`
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	return &Config{
		App: AppConfig{
			Name: "conch",
		},
		Log: LogConfig{
			Level:  "debug",
			Output: "stdout",
		},
		Server: ServerConfig{
			WorkDir:  defaultWorkDir,
			StateDir: defaultStateDir,
		},
		Network: NetworkConfig{
			WarmPoolSize: netstack.DefaultWarmPoolSize,
			CNI: CNIConfig{
				PluginBinDirs: []string{netstack.DefaultCNIPluginBinDir},
				PluginConfDir: netstack.DefaultCNIPluginConfDir,
				CacheDir:      defaultStateDir + "/cni",
			},
		},
		Sandbox: SandboxConfig{
			VsockSignalRetry:   10 * time.Millisecond,
			VsockSignalTimeout: 60 * time.Second,
			RequestTimeout:     60 * time.Second,
			Backend:            DefaultSandboxBackend,
			DefaultSpec: SandboxSpec{
				VCPUNum: 2,
				VCPUMax: 2,
				RamMB:   2048,
			},
			Stratovirt: &VMMBinaryConfig{Binary: "/usr/bin/stratovirt"},
		},
		Volume: VolumeConfig{
			MaxMounts: 10,
			Backend:   defaultVolumeBackend,
			Virtiofs: VolumeVirtiofsConfig{
				Binary: "virtiofsd",
			},
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

	// Open the file once so the permission check applies to the same file that is read.
	file, err := os.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to inspect config file permissions: %w", err)
	}
	// Allow owner-only files and group read, while forbidding group write or
	// execute and every permission granted to other users.
	if fileInfo.Mode().Perm()&0o037 != 0 {
		return nil, fmt.Errorf(
			"config file %q has insecure permissions %04o",
			configPath,
			fileInfo.Mode().Perm(),
		)
	}

	// Parse YAML
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
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
	if cfg.Server.WorkDir == "" {
		cfg.Server.WorkDir = defaultCfg.Server.WorkDir
	}
	if cfg.Server.StateDir == "" {
		cfg.Server.StateDir = defaultCfg.Server.StateDir
	}
	if cfg.Network.WarmPoolSize == 0 {
		cfg.Network.WarmPoolSize = defaultCfg.Network.WarmPoolSize
	}
	if len(cfg.Network.CNI.PluginBinDirs) == 0 {
		cfg.Network.CNI.PluginBinDirs = defaultCfg.Network.CNI.PluginBinDirs
	}
	cfg.Network.CNI.CacheDir = filepath.Join(cfg.Server.StateDir, "cni")
	if cfg.Sandbox.VsockSignalRetry == 0 {
		cfg.Sandbox.VsockSignalRetry = defaultCfg.Sandbox.VsockSignalRetry
	}
	if cfg.Sandbox.VsockSignalTimeout == 0 {
		cfg.Sandbox.VsockSignalTimeout = defaultCfg.Sandbox.VsockSignalTimeout
	}
	if cfg.Sandbox.RequestTimeout == 0 {
		cfg.Sandbox.RequestTimeout = defaultCfg.Sandbox.RequestTimeout
	}
	if cfg.Sandbox.Backend == "" {
		cfg.Sandbox.Backend = defaultCfg.Sandbox.Backend
	}
	if cfg.Sandbox.Stratovirt == nil {
		cfg.Sandbox.Stratovirt = defaultCfg.Sandbox.Stratovirt
	}
	if cfg.Sandbox.DefaultSpec.VCPUNum == 0 {
		cfg.Sandbox.DefaultSpec.VCPUNum = defaultCfg.Sandbox.DefaultSpec.VCPUNum
	}
	if cfg.Sandbox.DefaultSpec.VCPUMax == 0 {
		cfg.Sandbox.DefaultSpec.VCPUMax = defaultCfg.Sandbox.DefaultSpec.VCPUMax
	}
	if cfg.Sandbox.DefaultSpec.RamMB == 0 {
		cfg.Sandbox.DefaultSpec.RamMB = defaultCfg.Sandbox.DefaultSpec.RamMB
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
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}
	if cfg.Server.WorkDir != "" {
		WorkDir = cfg.Server.WorkDir
	}

	return &cfg, nil
}

func validateConfig(cfg *Config) error {
	if !filepath.IsAbs(cfg.Server.WorkDir) {
		return fmt.Errorf("invalid server.work_dir=%q: must be an absolute path", cfg.Server.WorkDir)
	}
	if !filepath.IsAbs(cfg.Server.StateDir) {
		return fmt.Errorf("invalid server.state_dir=%q: must be an absolute path", cfg.Server.StateDir)
	}
	if cfg.Network.WarmPoolSize < 0 {
		return fmt.Errorf("invalid network.warm_pool_size=%d: must be greater than or equal to 0", cfg.Network.WarmPoolSize)
	}
	if cfg.Volume.MaxMounts < 0 {
		return fmt.Errorf("invalid volume.max_mounts=%d: must be greater than or equal to 0", cfg.Volume.MaxMounts)
	}
	templateName := strings.TrimSpace(cfg.Sandbox.DefaultSpec.TemplateName)
	templateID := strings.TrimSpace(cfg.Sandbox.DefaultSpec.TemplateID)
	if templateName != "" && templateID != "" {
		return fmt.Errorf("sandbox.default_spec.template_name and sandbox.default_spec.template_id are mutually exclusive")
	}
	if templateID != "" {
		parsed, err := digest.Parse(templateID)
		if err != nil {
			return fmt.Errorf("invalid sandbox.default_spec.template_id %q: %w", templateID, err)
		}
		templateID = parsed.String()
	}
	cfg.Sandbox.DefaultSpec.TemplateName = templateName
	cfg.Sandbox.DefaultSpec.TemplateID = templateID
	backend := strings.TrimSpace(cfg.Volume.Backend)
	if backend != "" && backend != defaultVolumeBackend {
		return fmt.Errorf("invalid volume.backend=%q: only %q is supported", cfg.Volume.Backend, defaultVolumeBackend)
	}
	if cfg.Sandbox.DefaultSpec.VCPUNum < 1 ||
		cfg.Sandbox.DefaultSpec.VCPUMax < cfg.Sandbox.DefaultSpec.VCPUNum ||
		cfg.Sandbox.DefaultSpec.VCPUNum > runtimeapi.SandboxMaxVCPU ||
		cfg.Sandbox.DefaultSpec.VCPUMax > runtimeapi.SandboxMaxVCPU {
		return fmt.Errorf("invalid sandbox.default_spec CPU configuration")
	}
	if cfg.Sandbox.DefaultSpec.RamMB < 1 || cfg.Sandbox.DefaultSpec.RamMB > runtimeapi.SandboxMaxRAMMB {
		return fmt.Errorf("invalid sandbox.default_spec.ram_mb=%d: must be between 1 and %d", cfg.Sandbox.DefaultSpec.RamMB, runtimeapi.SandboxMaxRAMMB)
	}
	if err := validateVMMBinaryConfig("cloud_hypervisor", cfg.Sandbox.CloudHypervisor); err != nil {
		return err
	}
	if err := validateVMMBinaryConfig("stratovirt", cfg.Sandbox.Stratovirt); err != nil {
		return err
	}
	vmmBinaries := cfg.Sandbox.BinaryPaths()
	if _, ok := vmmBinaries[cfg.Sandbox.Backend]; !ok {
		return fmt.Errorf("sandbox.backend %q is not configured", cfg.Sandbox.Backend)
	}
	return nil
}

func validateVMMBinaryConfig(name string, cfg *VMMBinaryConfig) error {
	if cfg == nil {
		return nil
	}
	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		return fmt.Errorf("vmm.%s.binary is required when vmm.%s is configured", name, name)
	}
	if !filepath.IsAbs(binary) {
		return fmt.Errorf("invalid vmm.%s.binary=%q: must be an absolute path", name, cfg.Binary)
	}
	cfg.Binary = filepath.Clean(binary)
	return nil
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

// GetServerUnixSocket returns the fixed API socket path under WorkDir.
func (c *Config) GetServerUnixSocket() string {
	if c == nil {
		return ""
	}
	return filepath.Join(c.Server.WorkDir, "conchd.sock")
}

func (c *Config) PIDFilePath() string { return filepath.Join(c.Server.WorkDir, "conchd.pid") }

func (c *Config) ContainerdRootDir() string { return filepath.Join(c.Server.StateDir, "containerd") }

func (c *Config) ContainerdStateDir() string { return filepath.Join(c.Server.WorkDir, "containerd") }

func (c *Config) VirtiofsRuntimeDir() string { return filepath.Join(c.Server.WorkDir, "sandboxes") }

func (c *Config) StatePath() string { return filepath.Join(c.Server.StateDir, "state.db") }

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
