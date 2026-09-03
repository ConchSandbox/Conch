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

const testDefaultTemplateName = "registry.example.com/conch/default:latest"
const testDefaultTemplateID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

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

func TestGetServerUnixSocket(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{WorkDir: "/run/conch-test"},
	}
	if got := cfg.GetServerUnixSocket(); got != "/run/conch-test/conchd.sock" {
		t.Errorf("GetServerUnixSocket() = %q, want /run/conch-test/conchd.sock", got)
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
			"server:\n  work_dir: /tmp/conch\n  state_dir: /tmp/conch-state\n" +
			"sandbox:\n  backend: cloud-hypervisor\n  default_spec:\n    template_name: " + testDefaultTemplateName + "\n    vcpu_num: 3\n    vcpu_max: 5\n    ram_mb: 2048\n  cloud_hypervisor:\n    binary: /opt/vmm/cloud-hypervisor\n  stratovirt:\n    binary: /opt/vmm/stratovirt\n" +
			"network:\n  warm_pool_size: 123\n" +
			"  cni:\n    plugin_bin_dirs:\n      - /custom/cni/bin\n",
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
	if cfg.Server.WorkDir != "/tmp/conch" {
		t.Errorf("LoadConfig().Server.WorkDir = %q, want %q", cfg.Server.WorkDir, "/tmp/conch")
	}
	if cfg.Server.StateDir != "/tmp/conch-state" {
		t.Errorf("LoadConfig().Server.StateDir = %q, want /tmp/conch-state", cfg.Server.StateDir)
	}
	if cfg.Network.WarmPoolSize != 123 {
		t.Errorf("LoadConfig().Network.WarmPoolSize = %d, want %d", cfg.Network.WarmPoolSize, 123)
	}
	if len(cfg.Network.CNI.PluginBinDirs) != 1 || cfg.Network.CNI.PluginBinDirs[0] != "/custom/cni/bin" {
		t.Errorf("LoadConfig().Network.CNI.PluginBinDirs = %v, want [/custom/cni/bin]", cfg.Network.CNI.PluginBinDirs)
	}
	if cfg.Network.CNI.CacheDir != "/tmp/conch-state/cni" {
		t.Errorf("LoadConfig().Network.CNI.CacheDir = %q, want /tmp/conch-state/cni", cfg.Network.CNI.CacheDir)
	}
	if cfg.Sandbox.CloudHypervisor == nil || cfg.Sandbox.CloudHypervisor.Binary != "/opt/vmm/cloud-hypervisor" {
		t.Errorf("LoadConfig().Sandbox.CloudHypervisor = %#v, want configured binary", cfg.Sandbox.CloudHypervisor)
	}
	if cfg.Sandbox.Stratovirt == nil || cfg.Sandbox.Stratovirt.Binary != "/opt/vmm/stratovirt" {
		t.Errorf("LoadConfig().Sandbox.Stratovirt = %#v, want configured binary", cfg.Sandbox.Stratovirt)
	}
	if cfg.Sandbox.Backend != "cloud-hypervisor" {
		t.Errorf("LoadConfig().Sandbox.Backend = %q, want cloud-hypervisor", cfg.Sandbox.Backend)
	}
	if cfg.Sandbox.DefaultSpec.TemplateName != testDefaultTemplateName {
		t.Errorf("LoadConfig().Sandbox.DefaultSpec.TemplateName = %q, want %q", cfg.Sandbox.DefaultSpec.TemplateName, testDefaultTemplateName)
	}
	if cfg.Sandbox.DefaultSpec.VCPUNum != 3 {
		t.Errorf("LoadConfig().Sandbox.DefaultSpec.VCPUNum = %d, want %d", cfg.Sandbox.DefaultSpec.VCPUNum, 3)
	}
	if cfg.Sandbox.DefaultSpec.VCPUMax != 5 {
		t.Errorf("LoadConfig().Sandbox.DefaultSpec.VCPUMax = %d, want %d", cfg.Sandbox.DefaultSpec.VCPUMax, 5)
	}
	if cfg.Sandbox.DefaultSpec.RamMB != 2048 {
		t.Errorf("LoadConfig().Sandbox.DefaultSpec.RamMB = %d, want %d", cfg.Sandbox.DefaultSpec.RamMB, 2048)
	}
}

func TestValidateConfigTemplateSelector(t *testing.T) {
	t.Run("accepts Template ID", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Sandbox.DefaultSpec.TemplateID = "  " + testDefaultTemplateID + "  "
		if err := validateConfig(cfg); err != nil {
			t.Fatalf("validateConfig() error = %v", err)
		}
		if cfg.Sandbox.DefaultSpec.TemplateID != testDefaultTemplateID {
			t.Fatalf("TemplateID = %q", cfg.Sandbox.DefaultSpec.TemplateID)
		}
	})

	t.Run("rejects Name and ID", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Sandbox.DefaultSpec.TemplateName = testDefaultTemplateName
		cfg.Sandbox.DefaultSpec.TemplateID = testDefaultTemplateID
		if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("validateConfig() error = %v", err)
		}
	})

	t.Run("rejects invalid ID", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Sandbox.DefaultSpec.TemplateID = "not-a-digest"
		if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "template_id") {
			t.Fatalf("validateConfig() error = %v", err)
		}
	})
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

func TestLoadConfigRejectsRemovedPathSettings(t *testing.T) {
	tests := []string{
		"server:\n  unix_socket: /tmp/conchd.sock\n",
		"server:\n  pid_file: /tmp/conchd.pid\n",
		"containerd:\n  root_dir: /tmp/containerd\n",
		"state:\n  path: /tmp/state.db\n",
		"network:\n  cni:\n    plugin_conf_dir: /tmp/cni\n",
		"volume:\n  virtiofs:\n    runtime_dir: /tmp/sandboxes\n",
	}
	for _, data := range tests {
		cfgPath := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(cfgPath, []byte(data), 0o640); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		if _, err := LoadConfig(cfgPath); err == nil || !strings.Contains(err.Error(), "field") {
			t.Fatalf("LoadConfig() error = %q, want removed path setting to be rejected", err)
		}
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
			name:    "relative work directory",
			data:    "server:\n  work_dir: runtime/conch\n",
			wantErr: "server.work_dir",
		},
		{
			name:    "relative state directory",
			data:    "server:\n  state_dir: state/conch\n",
			wantErr: "server.state_dir",
		},
		{
			name:    "negative default vcpu",
			data:    "sandbox:\n  default_spec:\n    vcpu_num: -1\n",
			wantErr: "sandbox.default_spec CPU",
		},
		{
			name:    "default vcpu max below vcpu",
			data:    "sandbox:\n  default_spec:\n    vcpu_num: 4\n    vcpu_max: 2\n",
			wantErr: "sandbox.default_spec CPU",
		},
		{
			name:    "negative default ram",
			data:    "sandbox:\n  default_spec:\n    ram_mb: -1\n",
			wantErr: "sandbox.default_spec.ram_mb",
		},
		{
			name:    "default vcpu exceeds maximum",
			data:    "sandbox:\n  default_spec:\n    vcpu_num: 65\n    vcpu_max: 65\n",
			wantErr: "sandbox.default_spec CPU",
		},
		{
			name:    "default ram exceeds maximum",
			data:    "sandbox:\n  default_spec:\n    ram_mb: 262145\n",
			wantErr: "sandbox.default_spec.ram_mb",
		},
		{
			name:    "unsupported volume backend",
			data:    "volume:\n  backend: 9p\n",
			wantErr: "volume.backend",
		},
		{
			name:    "cloud hypervisor missing binary",
			data:    "sandbox:\n  cloud_hypervisor: {}\n",
			wantErr: "vmm.cloud_hypervisor.binary is required",
		},
		{
			name:    "stratovirt missing binary",
			data:    "sandbox:\n  stratovirt: {}\n",
			wantErr: "vmm.stratovirt.binary is required",
		},
		{
			name:    "relative cloud hypervisor binary",
			data:    "sandbox:\n  cloud_hypervisor:\n    binary: bin/cloud-hypervisor\n",
			wantErr: "vmm.cloud_hypervisor.binary",
		},
		{
			name:    "default VMM is not configured",
			data:    "sandbox:\n  backend: cloud-hypervisor\n",
			wantErr: `sandbox.backend "cloud-hypervisor" is not configured`,
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
		{
			name:    "removed CNI interface field",
			data:    "network:\n  cni:\n    if_name: net1\n",
			wantErr: "field if_name not found",
		},
		{
			name:    "removed CNI cache directory field",
			data:    "network:\n  cni:\n    cache_dir: /tmp/cni\n",
			wantErr: "field cache_dir not found",
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
}

func TestDefaultConfigRuntimePaths(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Server.WorkDir != "/var/run/conch" {
		t.Errorf("DefaultConfig().Server.WorkDir = %q, want /var/run/conch", cfg.Server.WorkDir)
	}
	if cfg.Server.StateDir != "/var/lib/conch" {
		t.Errorf("DefaultConfig().Server.StateDir = %q, want /var/lib/conch", cfg.Server.StateDir)
	}
	if cfg.GetServerUnixSocket() != "/var/run/conch/conchd.sock" || cfg.PIDFilePath() != "/var/run/conch/conchd.pid" {
		t.Errorf("unexpected server runtime paths: socket=%q pid=%q", cfg.GetServerUnixSocket(), cfg.PIDFilePath())
	}
	if cfg.ContainerdRootDir() != "/var/lib/conch/containerd" || cfg.ContainerdStateDir() != "/var/run/conch/containerd" {
		t.Errorf("unexpected containerd paths: root=%q state=%q", cfg.ContainerdRootDir(), cfg.ContainerdStateDir())
	}
	if cfg.VirtiofsRuntimeDir() != "/var/run/conch/sandboxes" {
		t.Errorf("unexpected virtiofs runtime path: %q", cfg.VirtiofsRuntimeDir())
	}
	if cfg.Sandbox.CloudHypervisor != nil || cfg.Sandbox.Stratovirt == nil || cfg.Sandbox.Stratovirt.Binary != "/usr/bin/stratovirt" {
		t.Errorf("DefaultConfig().Sandbox.Stratovirt = %#v, want /usr/bin/stratovirt", cfg.Sandbox.Stratovirt)
	}
	if cfg.Sandbox.Backend != DefaultSandboxBackend {
		t.Errorf("DefaultConfig().Sandbox.Backend = %q, want %q", cfg.Sandbox.Backend, DefaultSandboxBackend)
	}
	if cfg.Sandbox.DefaultSpec.VCPUNum != 2 || cfg.Sandbox.DefaultSpec.VCPUMax != 2 || cfg.Sandbox.DefaultSpec.RamMB != 2048 {
		t.Errorf("DefaultConfig().Sandbox.DefaultSpec = %#v, want 2 vCPU / 2048 MiB", cfg.Sandbox.DefaultSpec)
	}
}

func TestDefaultSandboxBackendStaysStratovirt(t *testing.T) {
	if DefaultSandboxBackend != "stratovirt" {
		t.Fatalf("DefaultSandboxBackend = %q, want stratovirt", DefaultSandboxBackend)
	}
	if got := DefaultConfig().Sandbox.Backend; got != "stratovirt" {
		t.Fatalf("DefaultConfig().Sandbox.Backend = %q, want stratovirt", got)
	}
}
