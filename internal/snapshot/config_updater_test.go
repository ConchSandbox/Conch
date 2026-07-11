package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestUpdateSnapshotConfigPreservesPmemDevices(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "payload": {
    "kernel": "/old/kernel",
    "initramfs": "/old/initrd"
  },
  "memory": {
    "zones": [
      {
        "file": "/old/mem",
        "shared": true
      }
    ]
  },
  "pmem": [
    {
      "file": "/old/rootfs",
      "discard_writes": false,
      "pci_segment": 1
    }
  ],
  "platform": {
    "num_pci_segments": 2
  },
  "vsock": {
    "cid": 3,
    "socket": "/old/vsock"
  }
}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	updater := &configUpdater{}
	if err := updater.updateSnapshotConfig(configPath, "/new/kernel", "/new/initrd", "/new/mem", 9, "/new/vsock"); err != nil {
		t.Fatalf("updateSnapshotConfig() error = %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var config map[string]interface{}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}

	platform, ok := config["platform"].(map[string]interface{})
	if !ok {
		t.Fatal("platform config missing")
	}
	if got := int(platform["num_pci_segments"].(float64)); got != 2 {
		t.Fatalf("num_pci_segments = %d, want 2", got)
	}

	pmem := config["pmem"].([]interface{})
	first := pmem[0].(map[string]interface{})
	if got := first["file"].(string); got != "/old/rootfs" {
		t.Fatalf("pmem file = %q, want /old/rootfs", got)
	}
	if got := int(first["pci_segment"].(float64)); got != 1 {
		t.Fatalf("pmem pci_segment = %d, want 1", got)
	}
}

func TestUpdateSnapshotPmemPathsPreservesDeviceOptions(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "pmem": [
    {
      "file": "/old/rootfs",
      "discard_writes": false,
      "pci_segment": 1
    }
  ]
}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := (&configUpdater{}).updateSnapshotPmemPaths(configPath, []string{"/new/rootfs"}); err != nil {
		t.Fatalf("updateSnapshotPmemPaths() error = %v", err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var config map[string]interface{}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	device := config["pmem"].([]interface{})[0].(map[string]interface{})
	if got := device["file"].(string); got != "/new/rootfs" {
		t.Fatalf("pmem file = %q, want /new/rootfs", got)
	}
	if got := int(device["pci_segment"].(float64)); got != 1 {
		t.Fatalf("pci_segment = %d, want 1", got)
	}
}
