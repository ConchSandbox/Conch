package config

import (
	"testing"
	"github.com/openeuler/Conch/pkg/ulog"
)

func TestGetLogConfig(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *Config
		wantStdout bool
		wantPath   string
		wantErr   bool
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

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		level string
		want  ulog.LogLevel
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
