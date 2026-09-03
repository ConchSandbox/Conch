package netstack

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cnilibrary "github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/invoke"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
)

const cniPluginTimeout = 20 * time.Second

func cniContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, cniPluginTimeout)
}

type cniNetwork struct {
	config *cnilibrary.NetworkConfigList
	ifName string
}

func setHostLocalIPAMDataDir(network *cnilibrary.NetworkConfigList, dataDir string) error {
	if network == nil {
		return fmt.Errorf("CNI network config is required")
	}
	for _, plugin := range network.Plugins {
		if plugin == nil || len(plugin.Bytes) == 0 {
			continue
		}
		var config map[string]any
		if err := json.Unmarshal(plugin.Bytes, &config); err != nil {
			return fmt.Errorf("parse CNI plugin configuration: %w", err)
		}
		ipam, ok := config["ipam"].(map[string]any)
		if !ok || ipam["type"] != "host-local" {
			continue
		}
		ipam["dataDir"] = dataDir
		data, err := json.Marshal(config)
		if err != nil {
			return fmt.Errorf("encode CNI plugin configuration: %w", err)
		}
		plugin.Bytes = data
	}
	return nil
}

type libCNIBackend struct {
	client       *cnilibrary.CNIConfig
	outerNetwork cniNetwork
}

func newLibCNIBackend(cfg CNIManagerConfig) (*libCNIBackend, error) {
	client := cnilibrary.NewCNIConfigWithCacheDir(cfg.PluginBinDirs, cfg.CacheDir, &invoke.DefaultExec{
		RawExec:       &invoke.RawExec{Stderr: os.Stderr},
		PluginDecoder: version.PluginDecoder{},
	})

	outer, err := loadDefaultCNINetwork(cfg.PluginConfDir)
	if err != nil {
		return nil, err
	}
	if err := setHostLocalIPAMDataDir(outer, filepath.Join(cfg.CacheDir, "networks")); err != nil {
		return nil, err
	}

	outerNetwork := cniNetwork{config: outer, ifName: cniOuterInterfaceName}
	return &libCNIBackend{
		client:       client,
		outerNetwork: outerNetwork,
	}, nil
}

func loadDefaultCNINetwork(confDir string) (*cnilibrary.NetworkConfigList, error) {
	files, err := cnilibrary.ConfFiles(confDir, []string{".conf", ".conflist", ".json"})
	if err != nil {
		return nil, fmt.Errorf("read CNI config directory %s: %w", confDir, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no CNI network config found in %s", confDir)
	}
	sort.Strings(files)
	confFile := files[0]

	var confList *cnilibrary.NetworkConfigList
	if strings.HasSuffix(confFile, ".conflist") {
		confList, err = cnilibrary.ConfListFromFile(confFile)
	} else {
		var conf *cnilibrary.NetworkConfig
		conf, err = cnilibrary.ConfFromFile(confFile)
		if err == nil {
			if conf.Network == nil || conf.Network.Type == "" {
				return nil, fmt.Errorf("CNI network type not found in %s", confFile)
			}
			confList, err = cnilibrary.ConfListFromConf(conf)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("load CNI network config %s: %w", confFile, err)
	}
	if confList == nil || len(confList.Plugins) == 0 {
		return nil, fmt.Errorf("CNI config %s contains no plugins", confFile)
	}
	return confList, nil
}

type bridgePluginConfig struct {
	Bridge string `json:"bridge"`
}

func loadedBridgeNetwork(config *cnilibrary.NetworkConfigList) (string, string, error) {
	if config == nil {
		return "", "", fmt.Errorf("CNI returned no loaded configuration")
	}
	for _, plugin := range config.Plugins {
		if plugin == nil || plugin.Network == nil || plugin.Network.Type != "bridge" {
			continue
		}
		var bridge bridgePluginConfig
		if err := json.Unmarshal(plugin.Bytes, &bridge); err != nil {
			return "", "", fmt.Errorf("parse bridge plugin config: %w", err)
		}
		if strings.TrimSpace(bridge.Bridge) == "" {
			return "", "", fmt.Errorf("loaded bridge network %q has no bridge name", config.Name)
		}
		return config.Name, bridge.Bridge, nil
	}
	return "", "", fmt.Errorf("loaded CNI configuration has no bridge network")
}

func (b *libCNIBackend) Setup(ctx context.Context, containerID, netnsPath string) (*types100.Result, error) {
	pluginCtx, cancel := cniContext(ctx)
	defer cancel()

	network := b.outerNetwork
	result, err := b.client.AddNetworkList(pluginCtx, network.config, runtimeConf(containerID, netnsPath, network.ifName))
	if err != nil {
		return nil, fmt.Errorf("add CNI network %q: %w", network.config.Name, err)
	}
	current, err := types100.NewResultFromResult(result)
	if err != nil {
		return nil, fmt.Errorf("convert result from CNI network %q: %w", network.config.Name, err)
	}
	return current, nil
}

func (b *libCNIBackend) Remove(ctx context.Context, containerID, netnsPath string) error {
	pluginCtx, cancel := cniContext(ctx)
	defer cancel()

	network := b.outerNetwork
	if err := b.client.DelNetworkList(pluginCtx, network.config, runtimeConf(containerID, netnsPath, network.ifName)); err != nil {
		return fmt.Errorf("delete CNI network %q: %w", network.config.Name, err)
	}
	return nil
}

func (b *libCNIBackend) CachedAttachments() ([]cniAttachment, error) {
	attachments, err := b.client.GetCachedAttachments("")
	if err != nil {
		return nil, err
	}
	result := make([]cniAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment == nil {
			continue
		}
		result = append(result, cniAttachment{
			ContainerID:   attachment.ContainerID,
			NetworkName:   attachment.Network,
			InterfaceName: attachment.IfName,
			NetNS:         attachment.NetNS,
		})
	}
	return result, nil
}

func runtimeConf(containerID, netnsPath, ifName string) *cnilibrary.RuntimeConf {
	return &cnilibrary.RuntimeConf{
		ContainerID: containerID,
		NetNS:       netnsPath,
		IfName:      ifName,
	}
}
