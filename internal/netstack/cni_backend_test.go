package netstack

import (
	"os"
	"path/filepath"
	"testing"
)

const testBridgeCNIConfig = `{
  "cniVersion": "1.0.0",
  "name": "conch-test",
  "plugins": [{
    "type": "bridge",
    "bridge": "conch-test0",
    "ipam": {
      "type": "host-local",
      "subnet": "198.19.255.0/30"
    }
  }]
}`

func writeTestCNIConfig(t *testing.T, dir, name, config string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(config), 0o600); err != nil {
		t.Fatalf("write CNI config: %v", err)
	}
}

func TestLoadDefaultCNINetworkUsesFirstSortedConfig(t *testing.T) {
	confDir := t.TempDir()
	writeTestCNIConfig(t, confDir, "20-second.conf", `{
  "cniVersion": "1.0.0", "name": "second", "type": "bridge", "bridge": "second0"
}`)
	writeTestCNIConfig(t, confDir, "10-first.conflist", testBridgeCNIConfig)

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
	writeTestCNIConfig(t, confDir, "10-conch.conflist", testBridgeCNIConfig)

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
