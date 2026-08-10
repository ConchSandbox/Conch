package clh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/vmm/driver"
)

func TestPrepareRestoreUpdatesHostResourcesAndPreservesDeviceOptions(t *testing.T) {
	snapshotDir := t.TempDir()
	configPath := filepath.Join(snapshotDir, snapshotConfigFileName)
	if err := os.WriteFile(configPath, []byte(`{
  "payload": {"kernel": "/old/kernel", "initramfs": "/old/initrd"},
  "memory": {"zones": [
    {"id": "other", "file": "/other/mem", "shared": true},
    {"id": "mem0", "file": "/old/mem", "shared": true}
  ]},
  "pmem": [{"file": "/old/rootfs", "discard_writes": false, "pci_segment": 1}],
  "platform": {"num_pci_segments": 2},
  "vsock": {"cid": 3, "socket": "/old/vsock"}
}`), 0o640); err != nil {
		t.Fatal(err)
	}

	pmemPaths, err := PrepareRestore(RestoreResources{
		SnapshotPath:    snapshotDir,
		MemoryPath:      "/new/mem",
		KernelPath:      "/new/kernel",
		InitrdPath:      "/new/initrd",
		PmemPaths:       []string{"/new/delta.erofs", "/new/base.erofs"},
		VsockCID:        9,
		VsockSocketPath: "/new/vsock",
	})
	if err != nil {
		t.Fatalf("PrepareRestore() error = %v", err)
	}
	if len(pmemPaths) != 1 || pmemPaths[0] != "/new/base.erofs" {
		t.Fatalf("prepared pmem paths = %#v", pmemPaths)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	payload := config["payload"].(map[string]any)
	if payload["kernel"] != "/new/kernel" || payload["initramfs"] != "/new/initrd" {
		t.Fatalf("payload = %#v", payload)
	}
	zones := config["memory"].(map[string]any)["zones"].([]any)
	otherZone := zones[0].(map[string]any)
	if otherZone["file"] != "/other/mem" || otherZone["shared"] != true {
		t.Fatalf("unrelated memory zone = %#v", otherZone)
	}
	zone := zones[1].(map[string]any)
	if zone["file"] != "/new/mem" || zone["shared"] != false {
		t.Fatalf("memory zone = %#v", zone)
	}
	pmem := config["pmem"].([]any)[0].(map[string]any)
	if pmem["file"] != "/new/base.erofs" || pmem["discard_writes"] != false || int(pmem["pci_segment"].(float64)) != 1 {
		t.Fatalf("pmem = %#v", pmem)
	}
	vsock := config["vsock"].(map[string]any)
	if int(vsock["cid"].(float64)) != 9 || vsock["socket"] != "/new/vsock" {
		t.Fatalf("vsock = %#v", vsock)
	}
	platform := config["platform"].(map[string]any)
	if int(platform["num_pci_segments"].(float64)) != 2 {
		t.Fatalf("platform = %#v", platform)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v", info.Mode().Perm())
	}
}

func TestPrepareRestoreAllowsSnapshotWithoutPmemDevices(t *testing.T) {
	snapshotDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(snapshotDir, snapshotConfigFileName), []byte(`{"memory":{"size":268435456}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := PrepareRestore(RestoreResources{
		SnapshotPath: snapshotDir,
		MemoryPath:   "/new/mem",
		PmemPaths:    []string{"/new/rootfs"},
	})
	if err != nil {
		t.Fatalf("PrepareRestore() error = %v", err)
	}
	if len(paths) != 1 || paths[0] != "/new/rootfs" {
		t.Fatalf("prepared pmem paths = %#v", paths)
	}
}

func TestPrepareRestoreRejectsShortRootfsChain(t *testing.T) {
	snapshotDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(snapshotDir, snapshotConfigFileName), []byte(`{"pmem":[{},{}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := PrepareRestore(RestoreResources{
		SnapshotPath: snapshotDir,
		MemoryPath:   "/new/mem",
		PmemPaths:    []string{"/layer/base0.erofs"},
	})
	if err == nil || !strings.Contains(err.Error(), "less than snapshot device count") {
		t.Fatalf("PrepareRestore() error = %v", err)
	}
}

func TestCLHPrepareLaunchSkipsSnapshotConfigForColdBoot(t *testing.T) {
	client := NewCLHClient(0, filepath.Join(t.TempDir(), "clh-api.sock"), "/opt/vmm/cloud-hypervisor")
	if err := client.PrepareLaunch(&driver.ResourceArgs{
		SnapfilePath: filepath.Join(t.TempDir(), "missing-snapshot"),
	}, false); err != nil {
		t.Fatalf("PrepareLaunch() error = %v", err)
	}
	client.Cleanup()
}
