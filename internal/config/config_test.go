package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/openeuler/Conch/pkg/ulog"
)

func TestGetLogConfig(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *Config
		wantStdout bool
		wantPath   string
		wantErr    bool
	}{
		{
			name: "stdout mode",
			cfg: &Config{
				Log: LogConfig{Level: "info", Output: "stdout"},
			},
			wantStdout: true,
			wantPath:   "",
		},
		{
			name: "file mode",
			cfg: &Config{
				Log: LogConfig{Level: "debug", Output: "file"},
			},
			wantStdout: false,
			wantPath:   "/var/log/conchd/",
		},
		{
			name: "both mode",
			cfg: &Config{
				Log: LogConfig{Level: "warn", Output: "both"},
			},
			wantStdout: true,
			wantPath:   "/var/log/conchd/",
		},
		{
			name: "invalid mode",
			cfg: &Config{
				Log: LogConfig{Level: "info", Output: "invalid"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.GetLogConfig()
			if (err != nil) != tt.wantErr {
				t.Errorf("GetLogConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got.Stdout != tt.wantStdout {
					t.Errorf("GetLogConfig().Stdout = %v, want %v", got.Stdout, tt.wantStdout)
				}
				if got.OutputPath != tt.wantPath {
					t.Errorf("GetLogConfig().OutputPath = %q, want %q", got.OutputPath, tt.wantPath)
				}
			}
		})
	}
}

func TestGetServerAddress(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{Host: "127.0.0.1", Port: 9999},
	}
	expected := "127.0.0.1:9999"
	if got := cfg.GetServerAddress(); got != expected {
		t.Errorf("GetServerAddress() = %q, want %q", got, expected)
	}
}

func TestGetServerUnixSocket(t *testing.T) {
	socketPath := "/var/run/conchd/conchd.sock"
	cfg := &Config{
		Server: ServerConfig{UnixSocket: &socketPath},
	}
	if got := cfg.GetServerUnixSocket(); got != socketPath {
		t.Errorf("GetServerUnixSocket() = %q, want %q", got, socketPath)
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		level   string
		want    ulog.LogLevel
		wantErr bool
	}{
		{"debug", ulog.DebugLevel, false},
		{"info", ulog.InfoLevel, false},
		{"warn", ulog.WarnLevel, false},
		{"error", ulog.ErrorLevel, false},
		{"fatal", ulog.FatalLevel, false},
		{"invalid", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			got, err := parseLogLevel(tt.level)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseLogLevel() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseLogLevel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	data := []byte(
		"app:\n  name: conch-test\n" +
			"log:\n  level: debug\n  output: both\n" +
			"server:\n  host: 127.0.0.1\n  port: 4567\n  unix_socket: \"\"\n  pid_file: /tmp/conchd.pid\n" +
			"containerd:\n  namespace: conch\n" +
			"network:\n  pool_size: 123\n  dynamic_reservation: true\n  tap_ip: 192.168.100.10\n  tap_mask: 25\n",
	)
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if cfg.App.Name != "conch-test" {
		t.Errorf("LoadConfig().App.Name = %q, want %q", cfg.App.Name, "conch-test")
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("LoadConfig().Log.Level = %q, want %q", cfg.Log.Level, "debug")
	}
	if cfg.Log.Output != "both" {
		t.Errorf("LoadConfig().Log.Output = %q, want %q", cfg.Log.Output, "both")
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("LoadConfig().Server.Host = %q, want %q", cfg.Server.Host, "127.0.0.1")
	}
	if cfg.Server.Port != 4567 {
		t.Errorf("LoadConfig().Server.Port = %d, want %d", cfg.Server.Port, 4567)
	}
	if cfg.GetServerUnixSocket() != "" {
		t.Errorf("LoadConfig().Server.UnixSocket = %q, want empty", cfg.GetServerUnixSocket())
	}
	if cfg.Server.PIDFile != "/tmp/conchd.pid" {
		t.Errorf("LoadConfig().Server.PIDFile = %q, want %q", cfg.Server.PIDFile, "/tmp/conchd.pid")
	}
	if cfg.Network.PoolSize != 123 {
		t.Errorf("LoadConfig().Network.PoolSize = %d, want %d", cfg.Network.PoolSize, 123)
	}
	if !cfg.Network.DynamicReservation {
		t.Errorf("LoadConfig().Network.DynamicReservation = %v, want true", cfg.Network.DynamicReservation)
	}
	if cfg.Network.TapIP != "192.168.100.10" {
		t.Errorf("LoadConfig().Network.TapIP = %q, want %q", cfg.Network.TapIP, "192.168.100.10")
	}
	if cfg.Network.TapMask != 25 {
		t.Errorf("LoadConfig().Network.TapMask = %d, want %d", cfg.Network.TapMask, 25)
	}
	if cfg.Containerd.Namespace != "conch" {
		t.Errorf("LoadConfig().Containerd.Namespace = %q, want %q", cfg.Containerd.Namespace, "conch")
	}
}

func TestDefaultConfigNetworkTapSettings(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Network.TapIP != "192.168.100.2" {
		t.Errorf("DefaultConfig().Network.TapIP = %q, want %q", cfg.Network.TapIP, "192.168.100.2")
	}
	if cfg.Network.TapMask != 24 {
		t.Errorf("DefaultConfig().Network.TapMask = %d, want %d", cfg.Network.TapMask, 24)
	}
}

func TestDefaultConfigContainerdSettings(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Server.UnixSocket == nil || *cfg.Server.UnixSocket != "/var/run/conchd/conchd.sock" {
		t.Errorf("DefaultConfig().Server.UnixSocket = %v, want %q", cfg.Server.UnixSocket, "/var/run/conchd/conchd.sock")
	}
	if cfg.Server.PIDFile != "/var/run/conchd/conchd.pid" {
		t.Errorf("DefaultConfig().Server.PIDFile = %q, want %q", cfg.Server.PIDFile, "/var/run/conchd/conchd.pid")
	}
	if cfg.Containerd.Namespace != "conch" {
		t.Errorf("DefaultConfig().Containerd.Namespace = %q, want %q", cfg.Containerd.Namespace, "conch")
	}
}
