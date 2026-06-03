package netstack

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	cni "github.com/containerd/go-cni"
	"github.com/containernetworking/cni/pkg/types"
)

func TestNormalizeCNIManagerConfigDefaults(t *testing.T) {
	cfg := normalizeCNIManagerConfig(CNIManagerConfig{})

	if !reflect.DeepEqual(cfg.PluginBinDirs, []string{defaultCNIPluginBinDir}) {
		t.Fatalf("PluginBinDirs = %v, want [%s]", cfg.PluginBinDirs, defaultCNIPluginBinDir)
	}
	if cfg.PluginConfDir != defaultCNIPluginConfDir {
		t.Fatalf("PluginConfDir = %q, want %q", cfg.PluginConfDir, defaultCNIPluginConfDir)
	}
	if cfg.PluginMaxConf != defaultCNIPluginMaxConf {
		t.Fatalf("PluginMaxConf = %d, want %d", cfg.PluginMaxConf, defaultCNIPluginMaxConf)
	}
	if cfg.IfName != defaultCNIIfName {
		t.Fatalf("IfName = %q, want %q", cfg.IfName, defaultCNIIfName)
	}
	if cfg.MinNetworkCount != 2 {
		t.Fatalf("MinNetworkCount = %d, want 2", cfg.MinNetworkCount)
	}
}

func TestNormalizeCNIManagerConfigPreservesExplicitValues(t *testing.T) {
	in := CNIManagerConfig{
		PluginBinDirs:   []string{"/custom/bin"},
		PluginConfDir:   "/custom/net.d",
		PluginMaxConf:   3,
		IfName:          "net7",
		SetupSerially:   true,
		MinNetworkCount: 4,
	}
	got := normalizeCNIManagerConfig(in)

	if !reflect.DeepEqual(got, in) {
		t.Fatalf("normalizeCNIManagerConfig() = %#v, want %#v", got, in)
	}
}

func TestInterfacePrefix(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "default", in: "", want: defaultCNIInterfacePrefix},
		{name: "eth0", in: "eth0", want: "eth"},
		{name: "net17", in: "net17", want: "net"},
		{name: "digits only", in: "123", want: defaultCNIInterfacePrefix},
		{name: "no suffix", in: "sandbox", want: "sandbox"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := interfacePrefix(tt.in); got != tt.want {
				t.Fatalf("interfacePrefix(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCNIConfigFileBridgeNames(t *testing.T) {
	cfg := cniConfigFile{
		Type:   "bridge",
		Bridge: "cni-conch0",
		Plugins: []cniPluginConfig{
			{Type: "loopback"},
			{Type: "bridge", Bridge: "cni-conch1"},
			{Type: "bridge", Bridge: "cni-conch0"},
		},
	}

	got := cfg.bridgeNames()
	want := []string{"cni-conch0", "cni-conch1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bridgeNames() = %v, want %v", got, want)
	}
}

func TestCNIConfigFileHostLocalAllocDir(t *testing.T) {
	tests := []struct {
		name string
		cfg  cniConfigFile
		want string
	}{
		{
			name: "top level default data dir",
			cfg:  cniConfigFile{Name: "conch-bridge", IPAM: cniIPAMConfig{Type: "host-local"}},
			want: filepath.Join(defaultHostLocalIPAMDataDir, "conch-bridge"),
		},
		{
			name: "plugin custom data dir",
			cfg: cniConfigFile{
				Name: "conch-bridge",
				Plugins: []cniPluginConfig{
					{Type: "bridge", IPAM: cniIPAMConfig{Type: "host-local", DataDir: "/tmp/cni-state"}},
				},
			},
			want: filepath.Join("/tmp/cni-state", "conch-bridge"),
		},
		{
			name: "no host local",
			cfg:  cniConfigFile{Name: "conch-bridge", IPAM: cniIPAMConfig{Type: "static"}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.hostLocalAllocDir(); got != tt.want {
				t.Fatalf("hostLocalAllocDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSelectedCNIConfigMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "00-other.conf"), []byte(`{"name":"other","type":"bridge","bridge":"other0"}`), 0o600); err != nil {
		t.Fatalf("write other config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "10-conch.conf"), []byte(`{
		"name": "conch-bridge",
		"type": "bridge",
		"bridge": "cni-conch0",
		"ipam": {"type":"host-local","dataDir":"/tmp/conch-cni"}
	}`), 0o600); err != nil {
		t.Fatalf("write selected config: %v", err)
	}

	got, err := selectedCNIConfigMetadata(dir, "conch-bridge")
	if err != nil {
		t.Fatalf("selectedCNIConfigMetadata() error = %v", err)
	}
	if !reflect.DeepEqual(got.bridgeNames, []string{"cni-conch0"}) {
		t.Fatalf("bridgeNames = %v, want [cni-conch0]", got.bridgeNames)
	}
	wantAllocDir := filepath.Join("/tmp/conch-cni", "conch-bridge")
	if got.hostLocalAllocDir != wantAllocDir {
		t.Fatalf("hostLocalAllocDir = %q, want %q", got.hostLocalAllocDir, wantAllocDir)
	}
}

func TestConvertCNIResult(t *testing.T) {
	_, defaultRoute, err := net.ParseCIDR("0.0.0.0/0")
	if err != nil {
		t.Fatal(err)
	}
	result := &cni.Result{
		Interfaces: map[string]*cni.Config{
			"eth0": {
				Mac:     "02:00:00:00:00:01",
				Sandbox: "/var/run/netns/ns-2",
				IPConfigs: []*cni.IPConfig{
					{IP: net.ParseIP("fd00::2"), Gateway: net.ParseIP("fd00::1")},
					{IP: net.ParseIP("10.12.0.2"), Gateway: net.ParseIP("10.12.0.1")},
				},
			},
			"host": {Mac: "02:00:00:00:00:02"},
		},
		Routes: []*types.Route{{Dst: *defaultRoute, GW: net.ParseIP("10.12.0.1")}},
		DNS: []types.DNS{{
			Nameservers: []string{"1.1.1.1"},
			Domain:      "example.test",
			Search:      []string{"svc.cluster.local"},
			Options:     []string{"ndots:5"},
		}},
	}

	got, err := convertCNIResult(result, "eth0")
	if err != nil {
		t.Fatalf("convertCNIResult() error = %v", err)
	}
	if got.IP != "10.12.0.2" {
		t.Fatalf("IP = %q, want 10.12.0.2", got.IP)
	}
	if !reflect.DeepEqual(got.AdditionalIPs, []string{"fd00::2"}) {
		t.Fatalf("AdditionalIPs = %v, want [fd00::2]", got.AdditionalIPs)
	}
	if len(got.Interfaces) != 2 {
		t.Fatalf("Interfaces len = %d, want 2", len(got.Interfaces))
	}
	if len(got.Routes) != 1 || got.Routes[0].Dst != "0.0.0.0/0" || got.Routes[0].GW != "10.12.0.1" {
		t.Fatalf("Routes = %#v, want default route via 10.12.0.1", got.Routes)
	}
	if len(got.DNS) != 1 || got.DNS[0].Nameservers[0] != "1.1.1.1" {
		t.Fatalf("DNS = %#v, want nameserver 1.1.1.1", got.DNS)
	}
}

func TestConvertCNIResultFallbackAndErrors(t *testing.T) {
	result := &cni.Result{
		Interfaces: map[string]*cni.Config{
			"net1": {IPConfigs: []*cni.IPConfig{{IP: net.ParseIP("10.12.0.8")}}},
		},
	}
	got, err := convertCNIResult(result, "eth0")
	if err != nil {
		t.Fatalf("convertCNIResult() fallback error = %v", err)
	}
	if got.IP != "10.12.0.8" {
		t.Fatalf("fallback IP = %q, want 10.12.0.8", got.IP)
	}

	if _, err := convertCNIResult(nil, "eth0"); err == nil {
		t.Fatalf("convertCNIResult(nil) error = nil, want error")
	}
	if _, err := convertCNIResult(&cni.Result{Interfaces: map[string]*cni.Config{"eth0": {}}}, "eth0"); err == nil {
		t.Fatalf("convertCNIResult(empty interface) error = nil, want error")
	}
}
