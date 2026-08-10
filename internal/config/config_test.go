package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/netstack"
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
			"server:\n  host: 127.0.0.1\n  port: 4567\n  unix_socket: \"\"\n  pid_file: /tmp/conchd.pid\n  work_dir: /tmp/conch\n" +
			"containerd:\n  root_dir: /tmp/conch-containerd-root\n  state_dir: /tmp/conch-containerd-state\n" +
			"vmm:\n  cloud_hypervisor:\n    binary: /opt/vmm/cloud-hypervisor\n  stratovirt:\n    binary: /opt/vmm/stratovirt\n" +
			"sandbox:\n  default_template_id: registry.example.invalid/conch/sandbox:latest\n  default_vmm_name: cloud-hypervisor\n  default_vcpu_num: 3\n  default_vcpu_max: 5\n  default_ram_mb: 2048\n" +
			"state:\n  path: /tmp/conch-state.db\n" +
			"network:\n  warm_pool_size: 123\n" +
			"  cni:\n    plugin_bin_dirs:\n      - /custom/cni/bin\n    plugin_conf_dir: /custom/cni/net.d\n    if_name: net1\n",
	)
	if err := os.WriteFile(cfgPath, data, 0640); err != nil {
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
	if cfg.Server.WorkDir != "/tmp/conch" {
		t.Errorf("LoadConfig().Server.WorkDir = %q, want %q", cfg.Server.WorkDir, "/tmp/conch")
	}
	if cfg.Network.WarmPoolSize != 123 {
		t.Errorf("LoadConfig().Network.WarmPoolSize = %d, want %d", cfg.Network.WarmPoolSize, 123)
	}
	if len(cfg.Network.CNI.PluginBinDirs) != 1 || cfg.Network.CNI.PluginBinDirs[0] != "/custom/cni/bin" {
		t.Errorf("LoadConfig().Network.CNI.PluginBinDirs = %v, want [/custom/cni/bin]", cfg.Network.CNI.PluginBinDirs)
	}
	if cfg.Network.CNI.PluginConfDir != "/custom/cni/net.d" {
		t.Errorf("LoadConfig().Network.CNI.PluginConfDir = %q, want %q", cfg.Network.CNI.PluginConfDir, "/custom/cni/net.d")
	}
	if cfg.Network.CNI.IfName != "net1" {
		t.Errorf("LoadConfig().Network.CNI.IfName = %q, want %q", cfg.Network.CNI.IfName, "net1")
	}
	if cfg.Containerd.RootDir != "/tmp/conch-containerd-root" {
		t.Errorf("LoadConfig().Containerd.RootDir = %q, want %q", cfg.Containerd.RootDir, "/tmp/conch-containerd-root")
	}
	if cfg.Containerd.StateDir != "/tmp/conch-containerd-state" {
		t.Errorf("LoadConfig().Containerd.StateDir = %q, want %q", cfg.Containerd.StateDir, "/tmp/conch-containerd-state")
	}
	if cfg.VMM.CloudHypervisor == nil || cfg.VMM.CloudHypervisor.Binary != "/opt/vmm/cloud-hypervisor" {
		t.Errorf("LoadConfig().VMM.CloudHypervisor = %#v, want configured binary", cfg.VMM.CloudHypervisor)
	}
	if cfg.VMM.Stratovirt == nil || cfg.VMM.Stratovirt.Binary != "/opt/vmm/stratovirt" {
		t.Errorf("LoadConfig().VMM.Stratovirt = %#v, want configured binary", cfg.VMM.Stratovirt)
	}
	if cfg.Sandbox.DefaultTemplateID != "registry.example.invalid/conch/sandbox:latest" {
		t.Errorf("LoadConfig().Sandbox.DefaultTemplateID = %q, want %q", cfg.Sandbox.DefaultTemplateID, "registry.example.invalid/conch/sandbox:latest")
	}
	if cfg.Sandbox.DefaultVMMName != "cloud-hypervisor" {
		t.Errorf("LoadConfig().Sandbox.DefaultVMMName = %q, want %q", cfg.Sandbox.DefaultVMMName, "cloud-hypervisor")
	}
	if cfg.Sandbox.DefaultVCPUNum != 3 {
		t.Errorf("LoadConfig().Sandbox.DefaultVCPUNum = %d, want %d", cfg.Sandbox.DefaultVCPUNum, 3)
	}
	if cfg.Sandbox.DefaultVCPUMax != 5 {
		t.Errorf("LoadConfig().Sandbox.DefaultVCPUMax = %d, want %d", cfg.Sandbox.DefaultVCPUMax, 5)
	}
	if cfg.Sandbox.DefaultRAMMB != 2048 {
		t.Errorf("LoadConfig().Sandbox.DefaultRAMMB = %d, want %d", cfg.Sandbox.DefaultRAMMB, 2048)
	}
	if cfg.State.Path != "/tmp/conch-state.db" {
		t.Errorf("LoadConfig().State.Path = %q, want %q", cfg.State.Path, "/tmp/conch-state.db")
	}
}

func TestLoadConfigRejectsRemovedCRISection(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("app:\n  name: conch-with-unused-config\ncri:\n  enabled: true\n  socket: /run/legacy-runtime.sock\n")
	if err := os.WriteFile(cfgPath, data, 0640); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := LoadConfig(cfgPath)
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want removed cri section to be rejected")
	}
	if !strings.Contains(err.Error(), "field cri not found") {
		t.Fatalf("LoadConfig() error = %q, want an unknown cri field error", err)
	}
}

func TestLoadConfigRejectsRemovedTapSettings(t *testing.T) {
	for _, field := range []string{"tap_ip", "tap_mask"} {
		t.Run(field, func(t *testing.T) {
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			data := []byte("network:\n  " + field + ": 1\n")
			if err := os.WriteFile(cfgPath, data, 0o640); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			_, err := LoadConfig(cfgPath)
			if err == nil || !strings.Contains(err.Error(), "field "+field+" not found") {
				t.Fatalf("LoadConfig() error = %q, want removed %s field error", err, field)
			}
		})
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{
			name:    "negative network pool size",
			data:    "network:\n  warm_pool_size: -1\n",
			wantErr: "network.warm_pool_size",
		},
		{
			name:    "negative volume max mounts",
			data:    "volume:\n  max_mounts: -1\n",
			wantErr: "volume.max_mounts",
		},
		{
			name:    "unsupported volume backend",
			data:    "volume:\n  backend: 9p\n",
			wantErr: "volume.backend",
		},
		{
			name:    "cloud hypervisor missing binary",
			data:    "vmm:\n  cloud_hypervisor: {}\n",
			wantErr: "vmm.cloud_hypervisor.binary is required",
		},
		{
			name:    "stratovirt missing binary",
			data:    "vmm:\n  stratovirt: {}\n",
			wantErr: "vmm.stratovirt.binary is required",
		},
		{
			name:    "relative cloud hypervisor binary",
			data:    "vmm:\n  cloud_hypervisor:\n    binary: bin/cloud-hypervisor\n",
			wantErr: "vmm.cloud_hypervisor.binary",
		},
		{
			name:    "default VMM is not configured",
			data:    "vmm:\n  cloud_hypervisor:\n    binary: /opt/vmm/cloud-hypervisor\n",
			wantErr: `sandbox.default_vmm_name "stratovirt" is not configured`,
		},
		{
			name:    "unknown top-level field",
			data:    "unknown_section:\n  enabled: true\n",
			wantErr: "field unknown_section not found",
		},
		{
			name:    "unknown nested field",
			data:    "network:\n  pool_szie: 12\n",
			wantErr: "field pool_szie not found",
		},
		{
			name:    "removed inherit host dns field",
			data:    "network:\n  inherit_host_dns: true\n",
			wantErr: "field inherit_host_dns not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(cfgPath, []byte(tt.data), 0640); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			_, err := LoadConfig(cfgPath)
			if err == nil {
				t.Fatalf("LoadConfig() error = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadConfig() error = %q, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigKeepsZeroValueDefaults(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("network:\n  warm_pool_size: 0\nvolume:\n  max_mounts: 0\n  backend: \"\"\n")
	if err := os.WriteFile(cfgPath, data, 0640); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	want := DefaultConfig()
	if cfg.Network.WarmPoolSize != want.Network.WarmPoolSize {
		t.Errorf("LoadConfig().Network.WarmPoolSize = %d, want default %d", cfg.Network.WarmPoolSize, want.Network.WarmPoolSize)
	}
	if cfg.Volume.MaxMounts != want.Volume.MaxMounts {
		t.Errorf("LoadConfig().Volume.MaxMounts = %d, want default %d", cfg.Volume.MaxMounts, want.Volume.MaxMounts)
	}
	if cfg.Volume.Backend != want.Volume.Backend {
		t.Errorf("LoadConfig().Volume.Backend = %q, want default %q", cfg.Volume.Backend, want.Volume.Backend)
	}
}

func TestLoadConfigRejectsInsecurePermissions(t *testing.T) {
	tests := []struct {
		name string
		mode os.FileMode
	}{
		{name: "group writable", mode: 0o660},
		{name: "group executable", mode: 0o610},
		{name: "other readable", mode: 0o604},
		{name: "world readable and writable", mode: 0o666},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			data := []byte("app:\n  name: shared-conch-config\n")
			if err := os.WriteFile(cfgPath, data, 0640); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			if err := os.Chmod(cfgPath, tt.mode); err != nil {
				t.Fatalf("Chmod(%#o) error = %v", tt.mode, err)
			}

			_, err := LoadConfig(cfgPath)
			if err == nil {
				t.Fatalf("LoadConfig() accepted config with permissions %#o", tt.mode)
			}
			if !strings.Contains(err.Error(), "insecure permissions") {
				t.Fatalf("LoadConfig() error = %q, want insecure permissions error", err)
			}
		})
	}
}

func TestLoadConfigAllowsGroupReadOnly(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o640} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(cfgPath, []byte("app:\n  name: secure-config\n"), mode); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}

			cfg, err := LoadConfig(cfgPath)
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			if cfg.App.Name != "secure-config" {
				t.Fatalf("LoadConfig().App.Name = %q, want secure-config", cfg.App.Name)
			}
		})
	}
}

func TestResolveCNIPluginConfDirFallback(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	localConfDir := filepath.Join(tmpDir, "cni", "net.d")
	defaultConfDir := filepath.Join(tmpDir, "etc", "conch", "cni", "net.d")
	if err := os.MkdirAll(localConfDir, 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(localConfDir, "10-conch.conf"), []byte("{}\n"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got := resolveCNIPluginConfDir(cfgPath, defaultConfDir, defaultConfDir)
	if got != localConfDir {
		t.Errorf("resolveCNIPluginConfDir() = %q, want %q", got, localConfDir)
	}
}

func TestDefaultConfigNetworkSettings(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Network.WarmPoolSize != netstack.DefaultWarmPoolSize {
		t.Errorf("DefaultConfig().Network.WarmPoolSize = %d, want %d", cfg.Network.WarmPoolSize, netstack.DefaultWarmPoolSize)
	}
	if len(cfg.Network.CNI.PluginBinDirs) != 1 || cfg.Network.CNI.PluginBinDirs[0] != netstack.DefaultCNIPluginBinDir {
		t.Errorf("DefaultConfig().Network.CNI.PluginBinDirs = %v, want [%s]", cfg.Network.CNI.PluginBinDirs, netstack.DefaultCNIPluginBinDir)
	}
	if cfg.Network.CNI.PluginConfDir != netstack.DefaultCNIPluginConfDir {
		t.Errorf("DefaultConfig().Network.CNI.PluginConfDir = %q, want %q", cfg.Network.CNI.PluginConfDir, netstack.DefaultCNIPluginConfDir)
	}
	if cfg.Network.CNI.IfName != netstack.DefaultCNIIfName {
		t.Errorf("DefaultConfig().Network.CNI.IfName = %q, want %s", cfg.Network.CNI.IfName, netstack.DefaultCNIIfName)
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
	if cfg.Server.WorkDir != "/var/run/conch" {
		t.Errorf("DefaultConfig().Server.WorkDir = %q, want %q", cfg.Server.WorkDir, "/var/run/conch")
	}
	if cfg.Containerd.RootDir != "/var/lib/conch/containerd" {
		t.Errorf("DefaultConfig().Containerd.RootDir = %q, want %q", cfg.Containerd.RootDir, "/var/lib/conch/containerd")
	}
	if cfg.Containerd.StateDir != "/run/conch/containerd" {
		t.Errorf("DefaultConfig().Containerd.StateDir = %q, want %q", cfg.Containerd.StateDir, "/run/conch/containerd")
	}
	if cfg.State.Path != "/var/lib/conch/state.db" {
		t.Errorf("DefaultConfig().State.Path = %q, want %q", cfg.State.Path, "/var/lib/conch/state.db")
	}
	if cfg.VMM.CloudHypervisor != nil || cfg.VMM.Stratovirt != nil {
		t.Errorf("DefaultConfig().VMM = %#v, want no configured VMM binaries", cfg.VMM)
	}
	if cfg.Sandbox.DefaultTemplateID != "" {
		t.Errorf("DefaultConfig().Sandbox.DefaultTemplateID = %q", cfg.Sandbox.DefaultTemplateID)
	}
	if cfg.Sandbox.DefaultVMMName != DefaultVMMName {
		t.Errorf("DefaultConfig().Sandbox.DefaultVMMName = %q, want %q", cfg.Sandbox.DefaultVMMName, DefaultVMMName)
	}
	if cfg.Sandbox.DefaultVCPUNum != 2 {
		t.Errorf("DefaultConfig().Sandbox.DefaultVCPUNum = %d, want 2", cfg.Sandbox.DefaultVCPUNum)
	}
	if cfg.Sandbox.DefaultVCPUMax != 2 {
		t.Errorf("DefaultConfig().Sandbox.DefaultVCPUMax = %d, want 2", cfg.Sandbox.DefaultVCPUMax)
	}
	if cfg.Sandbox.DefaultRAMMB != 4096 {
		t.Errorf("DefaultConfig().Sandbox.DefaultRAMMB = %d, want 4096", cfg.Sandbox.DefaultRAMMB)
	}
}

func TestDefaultVMMNameStaysStratovirt(t *testing.T) {
	if DefaultVMMName != "stratovirt" {
		t.Fatalf("DefaultVMMName = %q, want stratovirt", DefaultVMMName)
	}
	if got := DefaultConfig().Sandbox.DefaultVMMName; got != "stratovirt" {
		t.Fatalf("DefaultConfig().Sandbox.DefaultVMMName = %q, want stratovirt", got)
	}
}

func TestSandboxMemoryConfigDefaultsAndValidation(t *testing.T) {
	defaults := DefaultConfig()
	if defaults.Sandbox.MemoryMode != "full" {
		t.Fatalf("default memory mode = %q, want full", defaults.Sandbox.MemoryMode)
	}
	if defaults.Sandbox.CowSocket != "/run/conch/cow.sock" {
		t.Fatalf("default cow socket = %q", defaults.Sandbox.CowSocket)
	}
	if defaults.Sandbox.CowBinary != "/usr/bin/conch-cow" {
		t.Fatalf("default cow binary = %q", defaults.Sandbox.CowBinary)
	}

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("sandbox:\n  memory_mode: auto\n  cow_binary: /opt/conch/conch-cow\n  cow_socket: /tmp/custom-cow.sock\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sandbox.MemoryMode != "auto" || loaded.Sandbox.CowBinary != "/opt/conch/conch-cow" || loaded.Sandbox.CowSocket != "/tmp/custom-cow.sock" {
		t.Fatalf("loaded sandbox memory config = %#v", loaded.Sandbox)
	}

	relativeBinaryPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(relativeBinaryPath, []byte("sandbox:\n  cow_binary: bin/conch-cow\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(relativeBinaryPath); err == nil || !strings.Contains(err.Error(), "sandbox.cow_binary") {
		t.Fatalf("LoadConfig(relative cow_binary) error = %v, want sandbox.cow_binary", err)
	}

	for _, value := range []string{"unknown", "FULL"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("sandbox:\n  memory_mode: "+value+"\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "sandbox.memory_mode") {
				t.Fatalf("LoadConfig() error = %v, want sandbox.memory_mode", err)
			}
		})
	}

	legacyPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(legacyPath, []byte("sandbox:\n  memd_socket: /run/conch/memd.sock\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(legacyPath); err == nil || !strings.Contains(err.Error(), "field memd_socket not found") {
		t.Fatalf("LoadConfig(legacy memd_socket) error = %v", err)
	}
}
