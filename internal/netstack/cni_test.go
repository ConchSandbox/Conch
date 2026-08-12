package netstack

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	cnilibrary "github.com/containernetworking/cni/libcni"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	types100 "github.com/containernetworking/cni/pkg/types/100"
)

func TestNormalizeCNIManagerConfigDefaults(t *testing.T) {
	cfg := normalizeCNIManagerConfig(CNIManagerConfig{})

	if !reflect.DeepEqual(cfg.PluginBinDirs, []string{defaultCNIPluginBinDir}) {
		t.Fatalf("PluginBinDirs = %v, want [%s]", cfg.PluginBinDirs, defaultCNIPluginBinDir)
	}
	if cfg.PluginConfDir != defaultCNIPluginConfDir {
		t.Fatalf("PluginConfDir = %q, want %q", cfg.PluginConfDir, defaultCNIPluginConfDir)
	}
}

func TestExtractCNIDNS(t *testing.T) {
	result := &types100.Result{DNS: cnitypes.DNS{
		Nameservers: []string{"10.0.0.53", "10.0.0.54"},
		Search:      []string{"one.example", "two.example"},
		Options:     []string{"timeout:2"},
		Domain:      "ignored.example",
	}}
	got, err := extractCNIDNS(result)
	if err != nil {
		t.Fatalf("extractCNIDNS() error = %v", err)
	}
	if !reflect.DeepEqual(got.Nameservers, []string{"10.0.0.53", "10.0.0.54"}) ||
		!reflect.DeepEqual(got.Search, []string{"one.example", "two.example"}) ||
		got.Domain != "" {
		t.Fatalf("extractCNIDNS() = %#v", got)
	}
}

func TestExtractCNIDNSRejectsInvalidExplicitServer(t *testing.T) {
	result := &types100.Result{DNS: cnitypes.DNS{Nameservers: []string{"127.0.0.53"}}}
	if _, err := extractCNIDNS(result); err == nil {
		t.Fatal("extractCNIDNS() error = nil, want invalid CNI DNS error")
	}
}

func TestNormalizeCNIManagerConfigPreservesExplicitValues(t *testing.T) {
	in := CNIManagerConfig{
		PluginBinDirs: []string{"/custom/bin"},
		PluginConfDir: "/custom/net.d",
	}
	got := normalizeCNIManagerConfig(in)

	if !reflect.DeepEqual(got, in) {
		t.Fatalf("normalizeCNIManagerConfig() = %#v, want %#v", got, in)
	}
}

func TestLoadedBridgeNetwork(t *testing.T) {
	tests := []struct {
		name        string
		config      *cnilibrary.NetworkConfigList
		wantNetwork string
		wantBridge  string
		wantErr     string
	}{
		{
			name: "reads loaded bridge plugin",
			config: &cnilibrary.NetworkConfigList{
				Name: "custom-network",
				Plugins: []*cnilibrary.PluginConfig{{
					Network: &cnitypes.PluginConf{Type: "bridge"},
					Bytes:   []byte(`{"bridge":"custom-bridge"}`),
				}},
			},
			wantNetwork: "custom-network",
			wantBridge:  "custom-bridge",
		},
		{
			name:    "rejects missing configuration",
			config:  nil,
			wantErr: "no loaded configuration",
		},
		{
			name: "rejects missing bridge plugin",
			config: &cnilibrary.NetworkConfigList{
				Name:    "custom-network",
				Plugins: []*cnilibrary.PluginConfig{{Network: &cnitypes.PluginConf{Type: "host-local"}}},
			},
			wantErr: "no bridge network",
		},
		{
			name: "rejects missing bridge name",
			config: &cnilibrary.NetworkConfigList{
				Name: "custom-network",
				Plugins: []*cnilibrary.PluginConfig{{
					Network: &cnitypes.PluginConf{Type: "bridge"},
					Bytes:   []byte(`{}`),
				}},
			},
			wantErr: "has no bridge name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotNetwork, gotBridge, err := loadedBridgeNetwork(tt.config)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("loadedBridgeNetwork() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadedBridgeNetwork() error = %v", err)
			}
			if gotNetwork != tt.wantNetwork || gotBridge.Bridge != tt.wantBridge {
				t.Fatalf("loadedBridgeNetwork() = (%q, %q), want (%q, %q)", gotNetwork, gotBridge.Bridge, tt.wantNetwork, tt.wantBridge)
			}
		})
	}
}

func TestParseBridgeSubnet(t *testing.T) {
	bridge := bridgePluginConfig{Bridge: "cni-conch0"}
	bridge.IPAM.Subnet = "10.12.1.7/20"
	subnet, err := parseBridgeSubnet(bridge, "conch-bridge")
	if err != nil {
		t.Fatalf("parseBridgeSubnet() error = %v", err)
	}
	if want := netip.MustParsePrefix("10.12.0.0/20"); subnet != want {
		t.Fatalf("parseBridgeSubnet() = %s, want %s", subnet, want)
	}
}

func TestParseBridgeSubnetRejectsUnsupportedValues(t *testing.T) {
	for _, subnet := range []string{"", "not-a-subnet", "fd00::/64"} {
		bridge := bridgePluginConfig{Bridge: "cni-conch0"}
		bridge.IPAM.Subnet = subnet
		if _, err := parseBridgeSubnet(bridge, "conch-bridge"); err == nil {
			t.Fatalf("parseBridgeSubnet(%q) error = nil, want invalid subnet error", subnet)
		}
	}
}

func TestValidateCNISubnetOnHost(t *testing.T) {
	subnet := netip.MustParsePrefix("10.12.0.0/20")
	tests := []struct {
		name     string
		prefixes []hostNetworkPrefix
		wantErr  string
	}{
		{name: "no overlap", prefixes: []hostNetworkPrefix{{prefix: netip.MustParsePrefix("192.168.1.0/24"), interfaceName: "eth0"}}},
		{name: "overlap", prefixes: []hostNetworkPrefix{{prefix: netip.MustParsePrefix("10.12.4.0/24"), interfaceName: "vpn0"}}, wantErr: "conflicts with host prefix 10.12.4.0/24"},
		{name: "own bridge is ignored", prefixes: []hostNetworkPrefix{{prefix: subnet, interfaceName: "cni-conch0"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCNISubnetOnHost(subnet, "cni-conch0", func() ([]hostNetworkPrefix, error) {
				return tt.prefixes, nil
			})
			if tt.wantErr == "" && err != nil {
				t.Fatalf("validateCNISubnetOnHost() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("validateCNISubnetOnHost() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateCNISubnetOnHostPropagatesEnumerationFailure(t *testing.T) {
	wantErr := errors.New("netlink unavailable")
	err := validateCNISubnetOnHost(netip.MustParsePrefix("10.12.0.0/20"), "cni-conch0", func() ([]hostNetworkPrefix, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("validateCNISubnetOnHost() error = %v, want %v", err, wantErr)
	}
}

func TestExtractCNIIP(t *testing.T) {
	result := &types100.Result{
		Interfaces: []*types100.Interface{{Name: "eth0"}, {Name: "host"}},
		IPs: []*types100.IPConfig{
			{Interface: types100.Int(0), Address: net.IPNet{IP: net.ParseIP("fd00::2")}},
			{Interface: types100.Int(0), Address: net.IPNet{IP: net.ParseIP("10.12.0.2")}},
		},
	}

	got, err := extractCNIIP(result)
	if err != nil {
		t.Fatalf("extractCNIIP() error = %v", err)
	}
	if got != "10.12.0.2" {
		t.Fatalf("IP = %q, want 10.12.0.2", got)
	}
}

func TestExtractCNIIPRejectsOtherInterface(t *testing.T) {
	result := &types100.Result{
		Interfaces: []*types100.Interface{{Name: "net1"}},
		IPs:        []*types100.IPConfig{{Interface: types100.Int(0), Address: net.IPNet{IP: net.ParseIP("10.12.0.8")}}},
	}
	if _, err := extractCNIIP(result); err == nil {
		t.Fatal("extractCNIIP(other interface) error = nil, want error")
	}
}

func TestExtractCNIIPRejectsInvalidResults(t *testing.T) {
	if _, err := extractCNIIP(nil); err == nil {
		t.Fatal("extractCNIIP(nil) error = nil, want error")
	}
	if _, err := extractCNIIP(&types100.Result{Interfaces: []*types100.Interface{{Name: cniOuterInterfaceName}}}); err == nil {
		t.Fatal("extractCNIIP(empty interface) error = nil, want error")
	}
	if _, err := extractCNIIP(&types100.Result{
		Interfaces: []*types100.Interface{{Name: cniOuterInterfaceName}},
		IPs:        []*types100.IPConfig{nil, {}},
	}); err == nil {
		t.Fatal("extractCNIIP(nil IP configs) error = nil, want error")
	}
	if _, err := extractCNIIP(&types100.Result{
		Interfaces: []*types100.Interface{{Name: cniOuterInterfaceName}},
		IPs:        []*types100.IPConfig{{Interface: types100.Int(1), Address: net.IPNet{IP: net.ParseIP("10.12.0.2")}}},
	}); err == nil {
		t.Fatal("extractCNIIP(invalid interface) error = nil, want error")
	}
}

func TestCNIManagerDelegatesTeardown(t *testing.T) {
	var removeID, removePath string
	manager := &CNIManager{backend: &fakeCNIBackend{
		remove: func(_ context.Context, id, path string) error {
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
