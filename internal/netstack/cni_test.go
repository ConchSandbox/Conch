package netstack

import (
	"context"
	"net"
	"reflect"
	"strings"
	"testing"

	cni "github.com/containerd/go-cni"
	cnitypes "github.com/containernetworking/cni/pkg/types"
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

func TestExtractCNIDNS(t *testing.T) {
	result := &cni.Result{DNS: []cnitypes.DNS{
		{Nameservers: []string{"10.0.0.53"}, Search: []string{"one.example"}, Options: []string{"timeout:2"}},
		{Nameservers: []string{"10.0.0.54"}, Search: []string{"two.example"}, Domain: "ignored.example"},
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
	result := &cni.Result{DNS: []cnitypes.DNS{{Nameservers: []string{"127.0.0.53"}}}}
	if _, err := extractCNIDNS(result); err == nil {
		t.Fatal("extractCNIDNS() error = nil, want invalid CNI DNS error")
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
