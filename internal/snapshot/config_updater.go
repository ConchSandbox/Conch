package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openeuler/Conch/internal/snapshot/common"
)

// configUpdater handles snapshot configuration file updates.
type configUpdater struct{}

// RecordPmemDeviceCount records the PMEM devices present when the VMM snapshot
// was created without changing the VMM-native snapshot configuration.
func RecordPmemDeviceCount(snapshotDir string, count int) error {
	if count < 0 {
		return fmt.Errorf("invalid pmem device count %d", count)
	}

	configPath := filepath.Join(snapshotDir, common.SnapshotConfigFileName)
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read snapshot config %s: %w", configPath, err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("unmarshal snapshot config %s: %w", configPath, err)
	}
	if config == nil {
		config = make(map[string]interface{})
	}
	config[common.SnapshotConfigPmemDeviceCount] = count

	updated, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot config %s: %w", configPath, err)
	}
	if err := os.WriteFile(configPath, updated, common.FileMode); err != nil {
		return fmt.Errorf("write snapshot config %s: %w", configPath, err)
	}
	return nil
}

// updateSnapshotConfig updates the snapshot configuration file with new paths.
// A nil pmemPaths leaves VMM-native PMEM configuration unchanged.
func (cu *configUpdater) updateSnapshotConfig(configFilePath, kernelPath, initrdPath, memoryPath string, pmemPaths []string, cid uint32, socketPath string) error {
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return fmt.Errorf("error open snapshot config file %s : %w", configFilePath, err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("error unmarshal snapshot config file %s : %w", configFilePath, err)
	}

	cu.updatePayloadPaths(config, kernelPath, initrdPath)
	cu.updateMemoryZone(config, memoryPath)
	if err := cu.updatePmemPaths(config, pmemPaths); err != nil {
		return fmt.Errorf("update snapshot pmem paths: %w", err)
	}
	cu.updateVsockConfig(config, cid, socketPath)
	updatedData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshal config: %w", err)
	}
	if err := os.WriteFile(configFilePath, updatedData, 0600); err != nil {
		return fmt.Errorf("error write snapshot config file %s : %w", configFilePath, err)
	}

	return nil
}

func (cu *configUpdater) updatePmemPaths(config map[string]interface{}, paths []string) error {
	if paths == nil {
		return nil
	}
	raw, ok := config["pmem"]
	if !ok {
		return nil
	}
	devices, ok := raw.([]interface{})
	if !ok {
		return fmt.Errorf("pmem must be an array")
	}
	if len(devices) != len(paths) {
		return fmt.Errorf("pmem device count %d does not match resolved path count %d", len(devices), len(paths))
	}
	for i, rawDevice := range devices {
		device, ok := rawDevice.(map[string]interface{})
		if !ok {
			return fmt.Errorf("pmem device %d must be an object", i)
		}
		if _, ok := device["file"].(string); !ok {
			return fmt.Errorf("pmem device %d has no file path", i)
		}
		device["file"] = paths[i]
	}
	return nil
}

func readSnapshotPmemDeviceCount(configFilePath string) (int, error) {
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return 0, fmt.Errorf("error open snapshot config file %s : %w", configFilePath, err)
	}

	var config struct {
		PmemDeviceCount *int `json:"pmem_device_count"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return 0, fmt.Errorf("error unmarshal snapshot config file %s : %w", configFilePath, err)
	}
	if config.PmemDeviceCount == nil {
		return 0, fmt.Errorf("snapshot config file %s has no pmem_device_count", configFilePath)
	}
	if *config.PmemDeviceCount < 0 {
		return 0, fmt.Errorf("snapshot config file %s has invalid pmem_device_count %d", configFilePath, *config.PmemDeviceCount)
	}
	return *config.PmemDeviceCount, nil
}

// updatePayloadPaths updates kernel and initramfs paths in payload section.
func (cu *configUpdater) updatePayloadPaths(config map[string]interface{}, kernelPath, initrdPath string) {
	payload, ok := config["payload"].(map[string]interface{})
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

// updateMemoryZone updates the first memory zone's file path and shared flag.
func (cu *configUpdater) updateMemoryZone(config map[string]interface{}, memoryPath string) {
	memory, ok := config["memory"].(map[string]interface{})
	if !ok {
		return
	}
	zones, ok := memory["zones"].([]interface{})
	if !ok || len(zones) == 0 {
		return
	}
	zoneMap, ok := zones[0].(map[string]interface{})
	if !ok {
		return
	}
	if oldMemoryPath, ok := zoneMap["file"].(string); ok && oldMemoryPath != "" {
		zoneMap["file"] = memoryPath
	}
	if _, ok := zoneMap["shared"].(bool); ok {
		zoneMap["shared"] = false
	}
}

// updateVsockConfig updates the vsock configuration with new cid and socket path.
func (cu *configUpdater) updateVsockConfig(config map[string]interface{}, cid uint32, socketPath string) {
	if cid == 0 || socketPath == "" {
		return
	}
	vsock, ok := config["vsock"].(map[string]interface{})
	if !ok {
		return
	}
	vsock["cid"] = cid
	vsock["socket"] = socketPath
}
