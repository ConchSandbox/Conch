package main

import (
	"fmt"
	"os"

	"github.com/openeuler/Conch/internal/config"
)

func loadConchConfig(configPath string) (*config.Config, error) {
	cfgPath := configPath
	if cfgPath == "" {
		cfgPath = config.FindConfigFile()
	}

	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func resolveContainerdRuntime(cfg *config.Config, addrOverride, namespaceOverride string) (string, string) {
	containerdAddr := cfg.Containerd.Socket
	if containerdAddr == "" {
		containerdAddr = defaultContainerdAddress
	}
	if v := os.Getenv("CONTAINERD_ADDRESS"); v != "" {
		containerdAddr = v
	}
	if addrOverride != "" {
		containerdAddr = addrOverride
	}

	namespace := cfg.Containerd.DefaultNamespace
	if namespaceOverride != "" {
		namespace = namespaceOverride
	}
	if namespace == "" {
		namespace = "default"
	}

	return containerdAddr, namespace
}

func printUnpackSummary(results map[string]string) {
	fmt.Println("------------------------------------------------------------")
	fmt.Println("All sub-images processed successfully. Summary:")
	for kind, sid := range results {
		fmt.Printf("Type: %-15s | SnapshotID: %s\n", kind, sid)
	}
}
