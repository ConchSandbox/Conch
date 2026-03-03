package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
	"github.com/openeuler/Conch/pkg/ulog"
)

// Config holds the application configuration
type Config struct {
	App    AppConfig    `yaml:"app"`
	Log    LogConfig    `yaml:"log"`
	Server ServerConfig `yaml:"server"`
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
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// DefaultConfig returns the default configuration
func DefaultConfig() *Config {
	return &Config{
		App: AppConfig{
			Name: "conch",
		},
		Log: LogConfig{
			Level:  "info",
			Output: "stdout",
		},
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 4063,
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
		"config/config.yaml",
		"/etc/conchd/config.yaml",
		"/etc/conch/config.yaml",
	}

	for _, loc := range locations {
		if _, err := os.Stat(loc); err == nil {
			absPath, _ := filepath.Abs(loc)
			return absPath
		}
	}

	return ""
}
