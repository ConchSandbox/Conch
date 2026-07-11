package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
)

// configUpdater handles snapshot configuration file updates.
type configUpdater struct{}

// updateSnapshotConfig updates the snapshot configuration file with new paths.
func (cu *configUpdater) updateSnapshotConfig(configFilePath, kernelPath, initrdPath, memoryPath string, cid uint32, socketPath string) error {
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

func (cu *configUpdater) updateSnapshotPmemPaths(configFilePath string, pmemPaths []string) error {
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return fmt.Errorf("error open snapshot config file %s : %w", configFilePath, err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("error unmarshal snapshot config file %s : %w", configFilePath, err)
	}
	pmem, ok := config["pmem"].([]interface{})
	if !ok || len(pmem) == 0 {
		return nil
	}
	if len(pmemPaths) != len(pmem) {
		return fmt.Errorf("snapshot pmem path count %d does not match device count %d", len(pmemPaths), len(pmem))
	}
	for i, path := range pmemPaths {
		device, ok := pmem[i].(map[string]interface{})
		if !ok {
			return fmt.Errorf("snapshot pmem device %d is invalid", i)
		}
		device["file"] = path
	}
	updatedData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("error marshal config: %w", err)
	}
	if err := os.WriteFile(configFilePath, updatedData, 0600); err != nil {
		return fmt.Errorf("error write snapshot config file %s : %w", configFilePath, err)
	}
	return nil
}

func readSnapshotPmemDeviceCount(configFilePath string) (int, error) {
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return 0, fmt.Errorf("error open snapshot config file %s : %w", configFilePath, err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return 0, fmt.Errorf("error unmarshal snapshot config file %s : %w", configFilePath, err)
	}
	pmem, ok := config["pmem"].([]interface{})
	if !ok {
		return 0, nil
	}
	return len(pmem), nil
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
