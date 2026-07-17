package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/openeuler/Conch/internal/config"
	"github.com/openeuler/Conch/pkg/ulog"
)

func LoadConchConfig(configPath string) (*config.Config, error) {
	cfgPath := configPath
	if cfgPath == "" {
		cfgPath = config.FindConfigFile()
	}
	return config.LoadConfig(cfgPath)
}

func ResolveConchAPIURL(apiURLOverride, addressAlias string) string {
	if apiURLOverride != "" {
		return apiURLOverride
	}
	return addressAlias
}

func ResolveConchNamespace(cfg *config.Config, namespaceOverride string) string {
	namespace := cfg.Containerd.DefaultNamespace
	if namespaceOverride != "" {
		namespace = namespaceOverride
	}
	if namespace == "" {
		namespace = "default"
	}
	return namespace
}

func ParseRegistryUser(user string) (string, string, error) {
	if user == "" {
		return "", "", nil
	}
	idx := strings.IndexByte(user, ':')
	if idx <= 0 || idx == len(user)-1 {
		return "", "", fmt.Errorf("invalid --user value %q, want username:password", user)
	}
	return user[:idx], user[idx+1:], nil
}

func InitUnpackLogger() error {
	cfg := ulog.Config{
		Level:      ulog.InfoLevel,
		OutputPath: "/var/log/conch/",
		Stdout:     true,
	}
	if err := ulog.Init(cfg); err == nil {
		return nil
	}

	if err := ulog.Init(ulog.Config{
		Level:  ulog.InfoLevel,
		Stdout: true,
	}); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "warning: falling back to stdout-only logging for conch image unpack")
	return nil
}

func PrintUnpackSummary(results map[string]string) {
	fmt.Println("------------------------------------------------------------")
	fmt.Println("All sub-images processed successfully. Summary:")
	for kind, sid := range results {
		fmt.Printf("Type: %-15s | SnapshotID: %s\n", kind, sid)
	}
}
