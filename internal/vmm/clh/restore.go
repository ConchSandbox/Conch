package clh

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const snapshotConfigFileName = "config.json"

// RestoreResources contains the host resources that may be rebound in a Cloud
// Hypervisor snapshot before restore. The snapshot package only mounts these
// paths; this package owns the CLH config schema.
type RestoreResources struct {
	SnapshotPath    string
	MemoryPath      string
	KernelPath      string
	InitrdPath      string
	PmemPaths       []string
	VsockCID        uint32
	VsockSocketPath string
}

// PrepareRestore rewrites the Cloud Hypervisor snapshot config and returns the
// PMEM paths matching the captured device topology.
func PrepareRestore(resources RestoreResources) ([]string, error) {
	if strings.TrimSpace(resources.SnapshotPath) == "" {
		return nil, fmt.Errorf("Cloud Hypervisor snapshot path is required")
	}
	if strings.TrimSpace(resources.MemoryPath) == "" {
		return nil, fmt.Errorf("Cloud Hypervisor memory backing path is required")
	}
	configPath := filepath.Join(resources.SnapshotPath, snapshotConfigFileName)
	config, err := readSnapshotConfig(configPath)
	if err != nil {
		return nil, err
	}

	pmemDeviceCount, err := snapshotPmemDeviceCount(config)
	if err != nil {
		return nil, err
	}
	pmemPaths, err := selectSnapshotRestorePmemFiles(resources.PmemPaths, pmemDeviceCount)
	if err != nil {
		return nil, err
	}
	if err := updateSnapshotPmemPaths(config, pmemPaths); err != nil {
		return nil, err
	}
	updatePayloadPaths(config, resources.KernelPath, resources.InitrdPath)
	if err := updateMemoryZone(config, resources.MemoryPath); err != nil {
		return nil, err
	}
	updateVsockConfig(config, resources.VsockCID, resources.VsockSocketPath)

	if err := writeSnapshotConfig(configPath, config); err != nil {
		return nil, err
	}
	return append([]string(nil), pmemPaths...), nil
}

func readSnapshotConfig(configPath string) (map[string]any, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("open Cloud Hypervisor snapshot config %s: %w", configPath, err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("unmarshal Cloud Hypervisor snapshot config %s: %w", configPath, err)
	}
	return config, nil
}

func writeSnapshotConfig(configPath string, config map[string]any) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal Cloud Hypervisor snapshot config: %w", err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		return fmt.Errorf("write Cloud Hypervisor snapshot config %s: %w", configPath, err)
	}
	if err := os.Chmod(configPath, 0o600); err != nil {
		return fmt.Errorf("set Cloud Hypervisor snapshot config mode %s: %w", configPath, err)
	}
	return nil
}

func snapshotPmemDeviceCount(config map[string]any) (int, error) {
	value, exists := config["pmem"]
	if !exists {
		return 0, nil
	}
	pmem, ok := value.([]any)
	if !ok {
		return 0, fmt.Errorf("Cloud Hypervisor snapshot pmem field is invalid")
	}
	return len(pmem), nil
}

func updateSnapshotPmemPaths(config map[string]any, pmemPaths []string) error {
	pmem, ok := config["pmem"].([]any)
	if !ok || len(pmem) == 0 {
		return nil
	}
	if len(pmemPaths) != len(pmem) {
		return fmt.Errorf("snapshot pmem path count %d does not match device count %d", len(pmemPaths), len(pmem))
	}
	for i, path := range pmemPaths {
		device, ok := pmem[i].(map[string]any)
		if !ok {
			return fmt.Errorf("snapshot pmem device %d is invalid", i)
		}
		device["file"] = path
	}
	return nil
}

func selectSnapshotRestorePmemFiles(files []string, deviceCount int) ([]string, error) {
	if deviceCount <= 0 || len(files) == deviceCount {
		return files, nil
	}
	if len(files) < deviceCount {
		return nil, fmt.Errorf("rootfs pmem file count %d is less than snapshot device count %d", len(files), deviceCount)
	}
	return files[len(files)-deviceCount:], nil
}

func updatePayloadPaths(config map[string]any, kernelPath, initrdPath string) {
	payload, ok := config["payload"].(map[string]any)
	if !ok {
		return
	}
	if oldKernel, ok := payload["kernel"].(string); ok && oldKernel != "" {
		payload["kernel"] = kernelPath
	}
	if oldInitrd, ok := payload["initramfs"].(string); ok && oldInitrd != "" {
		payload["initramfs"] = initrdPath
	}
}

func updateMemoryZone(config map[string]any, memoryPath string) error {
	memory, ok := config["memory"].(map[string]any)
	if !ok {
		return nil
	}
	zones, ok := memory["zones"].([]any)
	if !ok || len(zones) == 0 {
		return nil
	}
	var target map[string]any
	for i, value := range zones {
		zone, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("Cloud Hypervisor memory zone %d is invalid", i)
		}
		if id, _ := zone["id"].(string); id == "mem0" {
			if target != nil {
				return fmt.Errorf("Cloud Hypervisor snapshot has duplicate memory zone id mem0")
			}
			target = zone
		}
	}
	if target == nil && len(zones) == 1 {
		// Compatibility for legacy single-zone configs that did not record an ID.
		target, _ = zones[0].(map[string]any)
	}
	if target == nil {
		return fmt.Errorf("Cloud Hypervisor snapshot is missing memory zone id mem0")
	}
	if oldPath, ok := target["file"].(string); ok && oldPath != "" {
		target["file"] = memoryPath
	}
	// Preserve the existing behavior until the CLH shared/private continuous
	// checkpoint investigation is resolved.
	if _, ok := target["shared"].(bool); ok {
		target["shared"] = false
	}
	return nil
}

func updateVsockConfig(config map[string]any, cid uint32, socketPath string) {
	if cid == 0 || socketPath == "" {
		return
	}
	vsock, ok := config["vsock"].(map[string]any)
	if !ok {
		return
	}
	vsock["cid"] = cid
	vsock["socket"] = socketPath
}
