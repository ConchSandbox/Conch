package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

func TestRecordPmemDeviceCountPreservesSnapshotConfig(t *testing.T) {
	snapshotDir := t.TempDir()
	configPath := filepath.Join(snapshotDir, common.SnapshotConfigFileName)
	if err := os.WriteFile(configPath, []byte(`{
  "payload": {"kernel": "/old/kernel"},
  "pmem": [
    {"file": "/old/layer0.erofs", "discard_writes": true},
    {"file": "/old/layer1.erofs", "discard_writes": true}
  ]
}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := RecordPmemDeviceCount(snapshotDir, 2); err != nil {
		t.Fatalf("RecordPmemDeviceCount() error = %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var config struct {
		PmemDeviceCount int `json:"pmem_device_count"`
		Pmem            []struct {
			File          string `json:"file"`
			DiscardWrites bool   `json:"discard_writes"`
		} `json:"pmem"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if config.PmemDeviceCount != 2 {
		t.Fatalf("pmem_device_count = %d, want 2", config.PmemDeviceCount)
	}
	if len(config.Pmem) != 2 || config.Pmem[0].File != "/old/layer0.erofs" || !config.Pmem[0].DiscardWrites {
		t.Fatalf("pmem config changed unexpectedly: %+v", config.Pmem)
	}
}

func TestUpdateSnapshotConfigRewritesPmemPathsAndPreservesDeviceOptions(t *testing.T) {
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
      "file": "/old/layer0.erofs",
      "discard_writes": true,
      "pci_segment": 0
    },
    {
      "file": "/old/layer1.erofs",
      "discard_writes": true,
      "pci_segment": 1
    }
  ],
  "pmem_device_count": 2,
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
	pmemPaths := []string{"/new/layer0.erofs", "/new/layer1.erofs"}
	if err := updater.updateSnapshotConfig(configPath, "/new/kernel", "/new/initrd", "/new/mem", pmemPaths, 9, "/new/vsock"); err != nil {
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

	if got := int(config["pmem_device_count"].(float64)); got != 2 {
		t.Fatalf("pmem_device_count = %d, want 2", got)
	}

	pmem := config["pmem"].([]interface{})
	for i, path := range pmemPaths {
		device := pmem[i].(map[string]interface{})
		if got := device["file"].(string); got != path {
			t.Fatalf("pmem[%d].file = %q, want %q", i, got, path)
		}
		if got := device["discard_writes"].(bool); !got {
			t.Fatalf("pmem[%d].discard_writes = false, want true", i)
		}
		if got := int(device["pci_segment"].(float64)); got != i {
			t.Fatalf("pmem[%d].pci_segment = %d, want %d", i, got, i)
		}
	}
}

func TestUpdateSnapshotConfigRejectsPmemPathCountMismatch(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "pmem": [
    {"file": "/old/layer0.erofs", "discard_writes": true}
  ],
  "pmem_device_count": 1
}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	updater := &configUpdater{}
	err := updater.updateSnapshotConfig(
		configPath,
		"/new/kernel",
		"/new/initrd",
		"/new/mem",
		[]string{"/new/layer0.erofs", "/new/layer1.erofs"},
		9,
		"/new/vsock",
	)
	if err == nil || !strings.Contains(err.Error(), "pmem device count 1 does not match resolved path count 2") {
		t.Fatalf("updateSnapshotConfig() error = %v, want PMEM count mismatch", err)
	}
}

func TestUpdateSnapshotConfigLeavesPmemPathsUnchangedWhenNotProvided(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{
  "pmem": [
    {"file": "/old/layer0.erofs", "discard_writes": true}
  ],
  "pmem_device_count": 1
}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	updater := &configUpdater{}
	if err := updater.updateSnapshotConfig(configPath, "/new/kernel", "/new/initrd", "/new/mem", nil, 0, ""); err != nil {
		t.Fatalf("updateSnapshotConfig() error = %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var config struct {
		Pmem []struct {
			File string `json:"file"`
		} `json:"pmem"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if got := config.Pmem[0].File; got != "/old/layer0.erofs" {
		t.Fatalf("pmem[0].file = %q, want original path", got)
	}
}

func TestReadSnapshotPmemDeviceCount(t *testing.T) {
	tests := map[string]int{
		"zero":     0,
		"positive": 3,
	}

	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(configPath, []byte(`{"pmem_device_count":`+strconv.Itoa(want)+`}`), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			got, err := readSnapshotPmemDeviceCount(configPath)
			if err != nil {
				t.Fatalf("readSnapshotPmemDeviceCount() error = %v", err)
			}
			if got != want {
				t.Fatalf("readSnapshotPmemDeviceCount() = %d, want %d", got, want)
			}
		})
	}
}

func TestReadSnapshotPmemDeviceCountRejectsInvalidMetadata(t *testing.T) {
	tests := map[string]string{
		"missing":      `{}`,
		"negative":     `{"pmem_device_count":-1}`,
		"non-integer":  `{"pmem_device_count":1.5}`,
		"legacy array": `{"pmem":[{"file":"/old/rootfs"}]}`,
	}

	for name, config := range tests {
		t.Run(name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			if _, err := readSnapshotPmemDeviceCount(configPath); err == nil {
				t.Fatal("readSnapshotPmemDeviceCount() error = nil, want invalid metadata error")
			}
		})
	}
}
