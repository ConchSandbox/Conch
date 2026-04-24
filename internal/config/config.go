package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/openeuler/Conch/pkg/ulog"
	"gopkg.in/yaml.v3"
)

// Config holds the application configuration
type Config struct {
	App        AppConfig        `yaml:"app"`
	Log        LogConfig        `yaml:"log"`
	Server     ServerConfig     `yaml:"server"`
	Network    NetworkConfig    `yaml:"network"`
	Containerd ContainerdConfig `yaml:"containerd"`
	Image      ImageConfig      `yaml:"image"`
	Sandbox    SandboxConfig    `yaml:"sandbox"`
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
	PoolSize           int    `yaml:"pool_size"`
	DynamicReservation bool   `yaml:"dynamic_reservation"`
	BridgeCount        int    `yaml:"bridge_count"`
	TapIP              string `yaml:"tap_ip"`
	TapMask            int    `yaml:"tap_mask"`
}

// ContainerdConfig holds containerd connection configuration
type ContainerdConfig struct {
	Socket           string `yaml:"socket"`
	DefaultNamespace string `yaml:"default_namespace"`
}

// ImageConfig holds image workflow defaults.
type ImageConfig struct {
	DefaultKernelImage string `yaml:"default_kernel_image"`
}

const DefaultKernelImage = "hub.oepkgs.net/conch/kernel:6.6.0"

type SandboxConfig struct {
	VsockSignalRetry   time.Duration `yaml:"vsock_signal_retry"`
	VsockSignalTimeout time.Duration `yaml:"vsock_signal_timeout"`
	RequestTimeout     time.Duration `yaml:"request_timeout"`
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
		},
		Containerd: ContainerdConfig{
			Socket:           "/run/containerd/containerd.sock",
			DefaultNamespace: "default",
		},
		Image: ImageConfig{
			DefaultKernelImage: DefaultKernelImage,
		},
		Sandbox: SandboxConfig{
			VsockSignalRetry:   10 * time.Millisecond,
			VsockSignalTimeout: 60 * time.Second,
			RequestTimeout:     60 * time.Second,
		},
	}
}

// LoadConfig loads configuration from the specified file path
func LoadConfig(configPath string) (*Config, error) {
	// If config path is empty, use default config
	if configPath == "" {
		return DefaultConfig(), nil
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
	if cfg.Containerd.Socket == "" {
		cfg.Containerd.Socket = defaultCfg.Containerd.Socket
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

	return &cfg, nil
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
