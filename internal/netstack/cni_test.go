package netstack

import (
	"context"
	"net"
	"reflect"
	"strings"
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
	if cfg.IfName != defaultCNIIfName {
		t.Fatalf("IfName = %q, want %q", cfg.IfName, defaultCNIIfName)
	}
	if defaultCNIMinNetworkCount != defaultCNIPluginMaxConfNum+1 {
		t.Fatalf("defaultCNIMinNetworkCount = %d, want defaultCNIPluginMaxConfNum + 1", defaultCNIMinNetworkCount)
	}
}

func TestNormalizeCNIManagerConfigPreservesExplicitValues(t *testing.T) {
	in := CNIManagerConfig{
		PluginBinDirs: []string{"/custom/bin"},
		PluginConfDir: "/custom/net.d",
		IfName:        "net7",
	}
	got := normalizeCNIManagerConfig(in)

	if !reflect.DeepEqual(got, in) {
		t.Fatalf("normalizeCNIManagerConfig() = %#v, want %#v", got, in)
	}
}

func TestNewCNIManagerRejectsIncompatibleIfName(t *testing.T) {
	_, err := NewCNIManager(CNIManagerConfig{IfName: "net1"})
	if err == nil {
		t.Fatal("NewCNIManager() error = nil, want incompatible if_name error")
	}
	if !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("NewCNIManager() error = %v, want incompatible if_name error", err)
	}
}

func TestLoadedBridgeNetwork(t *testing.T) {
	tests := []struct {
		name       string
		config     *cni.ConfigResult
		wantBridge string
		wantErr    string
	}{
		{
			name: "reads loaded bridge plugin",
			config: &cni.ConfigResult{Networks: []*cni.ConfNetwork{{
				Config: &cni.NetworkConfList{
					Name: "custom-network",
					Plugins: []*cni.NetworkConf{{
						Network: &types.PluginConf{Type: "bridge"},
						Source:  `{"bridge":"custom-bridge"}`,
					}},
				},
			}}},
			wantBridge: "custom-bridge",
		},
		{
			name:    "rejects missing configuration",
			config:  nil,
			wantErr: "no loaded configuration",
		},
		{
			name: "rejects missing bridge plugin",
			config: &cni.ConfigResult{Networks: []*cni.ConfNetwork{{
				Config: &cni.NetworkConfList{
					Name:    "custom-network",
					Plugins: []*cni.NetworkConf{{Network: &types.PluginConf{Type: "host-local"}}},
				},
			}}},
			wantErr: "no bridge network",
		},
		{
			name: "rejects missing bridge name",
			config: &cni.ConfigResult{Networks: []*cni.ConfNetwork{{
				Config: &cni.NetworkConfList{
					Name: "custom-network",
					Plugins: []*cni.NetworkConf{{
						Network: &types.PluginConf{Type: "bridge"},
						Source:  `{}`,
					}},
				},
			}}},
			wantErr: "has no bridge name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBridge, err := loadedBridgeName(tt.config)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("loadedBridgeName() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadedBridgeName() error = %v", err)
			}
			if gotBridge != tt.wantBridge {
				t.Fatalf("loadedBridgeName() = %q, want %q", gotBridge, tt.wantBridge)
			}
		})
	}
}

func TestExtractCNIIP(t *testing.T) {
	result := &cni.Result{
		Interfaces: map[string]*cni.Config{
			"eth0": {
				IPConfigs: []*cni.IPConfig{
					{IP: net.ParseIP("fd00::2")},
					{IP: net.ParseIP("10.12.0.2")},
				},
			},
			"host": {},
		},
	}

	got, err := extractCNIIP(result, "eth0")
	if err != nil {
		t.Fatalf("extractCNIIP() error = %v", err)
	}
	if got != "10.12.0.2" {
		t.Fatalf("IP = %q, want 10.12.0.2", got)
	}
}

func TestExtractCNIIPFallbackAndErrors(t *testing.T) {
	result := &cni.Result{
		Interfaces: map[string]*cni.Config{
			"net1": {IPConfigs: []*cni.IPConfig{{IP: net.ParseIP("10.12.0.8")}}},
		},
	}
	got, err := extractCNIIP(result, "eth0")
	if err != nil {
		t.Fatalf("extractCNIIP() fallback error = %v", err)
	}
	if got != "10.12.0.8" {
		t.Fatalf("fallback IP = %q, want 10.12.0.8", got)
	}

	if _, err := extractCNIIP(nil, "eth0"); err == nil {
		t.Fatalf("extractCNIIP(nil) error = nil, want error")
	}
	if _, err := extractCNIIP(&cni.Result{Interfaces: map[string]*cni.Config{"eth0": {}}}, "eth0"); err == nil {
		t.Fatalf("extractCNIIP(empty interface) error = nil, want error")
	}
	if _, err := extractCNIIP(&cni.Result{Interfaces: map[string]*cni.Config{
		"eth0": {IPConfigs: []*cni.IPConfig{nil, {}}},
	}}, "eth0"); err == nil {
		t.Fatalf("extractCNIIP(nil IP configs) error = nil, want error")
	}
}

func TestCNIManagerDelegatesTeardown(t *testing.T) {
	var removeID, removePath string
	manager := &CNIManager{plugin: &fakeCNIPlugin{
		remove: func(_ context.Context, id, path string, _ ...cni.NamespaceOpts) error {
			removeID, removePath = id, path
			return nil
		},
	}}

	if err := manager.TeardownSandboxNetwork(context.Background(), "slot-2", "/run/conch/netns/slot-2"); err != nil {
		t.Fatalf("TeardownSandboxNetwork(): %v", err)
	}
	if removeID != "slot-2" || removePath != "/run/conch/netns/slot-2" {
		t.Fatalf("Remove identity = (%q, %q), want slot identity and netns path", removeID, removePath)
	}
}
