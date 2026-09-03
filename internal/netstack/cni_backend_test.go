package netstack

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	cnilibrary "github.com/containernetworking/cni/libcni"
)

func TestCNIContextUsesPluginTimeout(t *testing.T) {
	earliestDeadline := time.Now().Add(cniPluginTimeout)
	ctx, cancel := cniContext(context.Background())
	defer cancel()
	latestDeadline := time.Now().Add(cniPluginTimeout)

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("cniContext() has no deadline")
	}
	if deadline.Before(earliestDeadline) || deadline.After(latestDeadline) {
		t.Fatalf("cniContext() deadline = %v, want between %v and %v", deadline, earliestDeadline, latestDeadline)
	}
}

func TestCNIContextPreservesShorterCallerDeadline(t *testing.T) {
	parent, parentCancel := context.WithTimeout(context.Background(), time.Second)
	defer parentCancel()
	parentDeadline, _ := parent.Deadline()

	ctx, cancel := cniContext(parent)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.Equal(parentDeadline) {
		t.Fatalf("cniContext() deadline = %v, want caller deadline %v", deadline, parentDeadline)
	}
}

const testBridgeCNIConfig = `{
  "cniVersion": "1.0.0",
  "name": "conch-test",
  "type": "bridge",
  "bridge": "conch-test0"
}`

func writeTestCNIConfig(t *testing.T, dir, name, config string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(config), 0o600); err != nil {
		t.Fatalf("write CNI config: %v", err)
	}
}

func TestSetHostLocalIPAMDataDir(t *testing.T) {
	network := &cnilibrary.NetworkConfigList{Plugins: []*cnilibrary.PluginConfig{{
		Bytes: []byte(`{"type":"bridge","ipam":{"type":"host-local","dataDir":"/old"}}`),
	}}}
	if err := setHostLocalIPAMDataDir(network, "/state/conch/cni/networks"); err != nil {
		t.Fatalf("setHostLocalIPAMDataDir() error = %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(network.Plugins[0].Bytes, &config); err != nil {
		t.Fatalf("decode rewritten CNI config: %v", err)
	}
	ipam := config["ipam"].(map[string]any)
	if got := ipam["dataDir"]; got != "/state/conch/cni/networks" {
		t.Fatalf("host-local dataDir = %q, want /state/conch/cni/networks", got)
	}
}

func TestLoadDefaultCNINetworkUsesFirstSortedConfig(t *testing.T) {
	confDir := t.TempDir()
	writeTestCNIConfig(t, confDir, "20-second.conf", `{
  "cniVersion": "1.0.0", "name": "second", "type": "bridge", "bridge": "second0"
}`)
	writeTestCNIConfig(t, confDir, "10-first.conf", testBridgeCNIConfig)

	network, err := loadDefaultCNINetwork(confDir)
	if err != nil {
		t.Fatalf("loadDefaultCNINetwork(): %v", err)
	}
	if network.Name != "conch-test" {
		t.Fatalf("loaded network = %q, want conch-test", network.Name)
	}
}

func TestNewCNIManagerUsesInternalInterface(t *testing.T) {
	confDir := t.TempDir()
	writeTestCNIConfig(t, confDir, "10-conch.conf", testBridgeCNIConfig)

	manager, err := NewCNIManager(CNIManagerConfig{
		PluginBinDirs: []string{t.TempDir()},
		PluginConfDir: confDir,
	})
	if err != nil {
		t.Fatalf("NewCNIManager(): %v", err)
	}
	if manager.ifName != cniOuterInterfaceName {
		t.Fatalf("CNI interface = %q, want %q", manager.ifName, cniOuterInterfaceName)
	}
}
