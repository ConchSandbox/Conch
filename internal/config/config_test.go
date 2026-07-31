package config

import (
	"os"
	"path/filepath"
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
			"containerd:\n  root_dir: /tmp/conch-containerd-root\n  state_dir: /tmp/conch-containerd-state\n  default_namespace: team-a\n" +
			"image:\n  default_kernel_image: registry.example.invalid/conch/kernel:6.6.0\n  default_kernel_plain_http: true\n  default_kernel_registry_username: kernel-user\n  default_kernel_registry_password: kernel-pass\n" +
			"sandbox:\n  default_template_id: registry.example.invalid/conch/sandbox:latest\n  default_vmm_name: test-vmm\n  default_vcpu_num: 3\n  default_vcpu_max: 5\n  default_ram_mb: 2048\n" +
			"state:\n  path: /tmp/conch-state.db\n" +
			"network:\n  pool_size: 123\n  dynamic_reservation: true\n  bridge_count: 7\n  tap_ip: 192.168.100.10\n  tap_mask: 25\n" +
			"  cni:\n    plugin_bin_dirs:\n      - /custom/cni/bin\n    plugin_conf_dir: /custom/cni/net.d\n    plugin_max_conf: 2\n    if_name: net1\n    setup_serially: true\n",
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
	if cfg.Server.WorkDir != "/tmp/conch" {
		t.Errorf("LoadConfig().Server.WorkDir = %q, want %q", cfg.Server.WorkDir, "/tmp/conch")
	}
	if cfg.Network.PoolSize != 123 {
		t.Errorf("LoadConfig().Network.PoolSize = %d, want %d", cfg.Network.PoolSize, 123)
	}
	if !cfg.Network.DynamicReservation {
		t.Errorf("LoadConfig().Network.DynamicReservation = %v, want true", cfg.Network.DynamicReservation)
	}
	if cfg.Network.BridgeCount != 7 {
		t.Errorf("LoadConfig().Network.BridgeCount = %d, want %d", cfg.Network.BridgeCount, 7)
	}
	if cfg.Network.TapIP != "192.168.100.10" {
		t.Errorf("LoadConfig().Network.TapIP = %q, want %q", cfg.Network.TapIP, "192.168.100.10")
	}
	if cfg.Network.TapMask != 25 {
		t.Errorf("LoadConfig().Network.TapMask = %d, want %d", cfg.Network.TapMask, 25)
	}
	if len(cfg.Network.CNI.PluginBinDirs) != 1 || cfg.Network.CNI.PluginBinDirs[0] != "/custom/cni/bin" {
		t.Errorf("LoadConfig().Network.CNI.PluginBinDirs = %v, want [/custom/cni/bin]", cfg.Network.CNI.PluginBinDirs)
	}
	if cfg.Network.CNI.PluginConfDir != "/custom/cni/net.d" {
		t.Errorf("LoadConfig().Network.CNI.PluginConfDir = %q, want %q", cfg.Network.CNI.PluginConfDir, "/custom/cni/net.d")
	}
	if cfg.Network.CNI.PluginMaxConf != 2 {
		t.Errorf("LoadConfig().Network.CNI.PluginMaxConf = %d, want 2", cfg.Network.CNI.PluginMaxConf)
	}
	if cfg.Network.CNI.IfName != "net1" {
		t.Errorf("LoadConfig().Network.CNI.IfName = %q, want %q", cfg.Network.CNI.IfName, "net1")
	}
	if !cfg.Network.CNI.SetupSerially {
		t.Errorf("LoadConfig().Network.CNI.SetupSerially = %v, want true", cfg.Network.CNI.SetupSerially)
	}
	if cfg.Containerd.RootDir != "/tmp/conch-containerd-root" {
		t.Errorf("LoadConfig().Containerd.RootDir = %q, want %q", cfg.Containerd.RootDir, "/tmp/conch-containerd-root")
	}
	if cfg.Containerd.StateDir != "/tmp/conch-containerd-state" {
		t.Errorf("LoadConfig().Containerd.StateDir = %q, want %q", cfg.Containerd.StateDir, "/tmp/conch-containerd-state")
	}
	if cfg.Containerd.DefaultNamespace != "team-a" {
		t.Errorf("LoadConfig().Containerd.DefaultNamespace = %q, want %q", cfg.Containerd.DefaultNamespace, "team-a")
	}
	if cfg.Image.DefaultKernelImage != "registry.example.invalid/conch/kernel:6.6.0" {
		t.Errorf("LoadConfig().Image.DefaultKernelImage = %q, want %q", cfg.Image.DefaultKernelImage, "registry.example.invalid/conch/kernel:6.6.0")
	}
	if !cfg.Image.DefaultKernelPlainHTTP {
		t.Errorf("LoadConfig().Image.DefaultKernelPlainHTTP = false, want true")
	}
	if cfg.Image.DefaultKernelRegistryUsername != "kernel-user" {
		t.Errorf("LoadConfig().Image.DefaultKernelRegistryUsername = %q, want %q", cfg.Image.DefaultKernelRegistryUsername, "kernel-user")
	}
	if cfg.Image.DefaultKernelRegistryPassword != "kernel-pass" {
		t.Errorf("LoadConfig().Image.DefaultKernelRegistryPassword = %q, want %q", cfg.Image.DefaultKernelRegistryPassword, "kernel-pass")
	}
	if cfg.Sandbox.DefaultTemplateID != "registry.example.invalid/conch/sandbox:latest" {
		t.Errorf("LoadConfig().Sandbox.DefaultTemplateID = %q, want %q", cfg.Sandbox.DefaultTemplateID, "registry.example.invalid/conch/sandbox:latest")
	}
	if cfg.Sandbox.DefaultVMMName != "test-vmm" {
		t.Errorf("LoadConfig().Sandbox.DefaultVMMName = %q, want %q", cfg.Sandbox.DefaultVMMName, "test-vmm")
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

func TestLoadConfigIgnoresRemovedCRISection(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("app:\n  name: conch-with-unused-config\ncri:\n  enabled: true\n  socket: /run/legacy-runtime.sock\n")
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.App.Name != "conch-with-unused-config" {
		t.Fatalf("LoadConfig().App.Name = %q, want conch-with-unused-config", cfg.App.Name)
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

func TestDefaultConfigNetworkTapSettings(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Network.TapIP != "192.168.100.2" {
		t.Errorf("DefaultConfig().Network.TapIP = %q, want %q", cfg.Network.TapIP, "192.168.100.2")
	}
	if cfg.Network.BridgeCount != 1 {
		t.Errorf("DefaultConfig().Network.BridgeCount = %d, want %d", cfg.Network.BridgeCount, 1)
	}
	if cfg.Network.TapMask != 24 {
		t.Errorf("DefaultConfig().Network.TapMask = %d, want %d", cfg.Network.TapMask, 24)
	}
	if len(cfg.Network.CNI.PluginBinDirs) != 1 || cfg.Network.CNI.PluginBinDirs[0] != netstack.DefaultCNIPluginBinDir {
		t.Errorf("DefaultConfig().Network.CNI.PluginBinDirs = %v, want [%s]", cfg.Network.CNI.PluginBinDirs, netstack.DefaultCNIPluginBinDir)
	}
	if cfg.Network.CNI.PluginConfDir != netstack.DefaultCNIPluginConfDir {
		t.Errorf("DefaultConfig().Network.CNI.PluginConfDir = %q, want %q", cfg.Network.CNI.PluginConfDir, netstack.DefaultCNIPluginConfDir)
	}
	if cfg.Network.CNI.PluginMaxConf != netstack.DefaultCNIPluginMaxConf {
		t.Errorf("DefaultConfig().Network.CNI.PluginMaxConf = %d, want %d", cfg.Network.CNI.PluginMaxConf, netstack.DefaultCNIPluginMaxConf)
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
	if cfg.Containerd.DefaultNamespace != "default" {
		t.Errorf("DefaultConfig().Containerd.DefaultNamespace = %q, want %q", cfg.Containerd.DefaultNamespace, "default")
	}
	if cfg.Image.DefaultKernelImage != DefaultKernelImage {
		t.Errorf("DefaultConfig().Image.DefaultKernelImage = %q, want %q", cfg.Image.DefaultKernelImage, DefaultKernelImage)
	}
	if cfg.Image.DefaultKernelPlainHTTP {
		t.Errorf("DefaultConfig().Image.DefaultKernelPlainHTTP = true, want false")
	}
	if cfg.Image.DefaultKernelRegistryUsername != "" {
		t.Errorf("DefaultConfig().Image.DefaultKernelRegistryUsername = %q, want empty", cfg.Image.DefaultKernelRegistryUsername)
	}
	if cfg.Image.DefaultKernelRegistryPassword != "" {
		t.Errorf("DefaultConfig().Image.DefaultKernelRegistryPassword = %q, want empty", cfg.Image.DefaultKernelRegistryPassword)
	}
	if cfg.State.Path != "/var/lib/conch/state.db" {
		t.Errorf("DefaultConfig().State.Path = %q, want %q", cfg.State.Path, "/var/lib/conch/state.db")
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
