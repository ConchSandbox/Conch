package main

import (
	"fmt"

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

func resolveConchAPIURL(apiURLOverride, addressAlias string) string {
	if apiURLOverride != "" {
		return apiURLOverride
	}
	return addressAlias
}

func resolveConchNamespace(cfg *config.Config, namespaceOverride string) string {
	namespace := cfg.Containerd.DefaultNamespace
	if namespaceOverride != "" {
		namespace = namespaceOverride
	}
	if namespace == "" {
		namespace = "default"
	}
	return namespace
}

func printUnpackSummary(results map[string]string) {
	fmt.Println("------------------------------------------------------------")
	fmt.Println("All sub-images processed successfully. Summary:")
	for kind, sid := range results {
		fmt.Printf("Type: %-15s | SnapshotID: %s\n", kind, sid)
	}
}
