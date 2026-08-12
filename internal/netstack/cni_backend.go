package netstack

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	cnilibrary "github.com/containernetworking/cni/libcni"
	"github.com/containernetworking/cni/pkg/invoke"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
)

type cniNetwork struct {
	config *cnilibrary.NetworkConfigList
	ifName string
}

type libCNIBackend struct {
	client       *cnilibrary.CNIConfig
	outerNetwork cniNetwork
}

func newLibCNIBackend(cfg CNIManagerConfig) (*libCNIBackend, error) {
	client := cnilibrary.NewCNIConfigWithCacheDir(cfg.PluginBinDirs, cniCacheDir, &invoke.DefaultExec{
		RawExec:       &invoke.RawExec{Stderr: os.Stderr},
		PluginDecoder: version.PluginDecoder{},
	})

	outer, err := loadDefaultCNINetwork(cfg.PluginConfDir)
	if err != nil {
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
	IPAM   struct {
		Subnet string `json:"subnet"`
	} `json:"ipam"`
}

func loadedBridgeNetwork(config *cnilibrary.NetworkConfigList) (string, bridgePluginConfig, error) {
	if config == nil {
		return "", bridgePluginConfig{}, fmt.Errorf("CNI returned no loaded configuration")
	}
	for _, plugin := range config.Plugins {
		if plugin == nil || plugin.Network == nil || plugin.Network.Type != "bridge" {
			continue
		}
		var bridge bridgePluginConfig
		if err := json.Unmarshal(plugin.Bytes, &bridge); err != nil {
			return "", bridgePluginConfig{}, fmt.Errorf("parse bridge plugin config: %w", err)
		}
		if strings.TrimSpace(bridge.Bridge) == "" {
			return "", bridgePluginConfig{}, fmt.Errorf("loaded bridge network %q has no bridge name", config.Name)
		}
		return config.Name, bridge, nil
	}
	return "", bridgePluginConfig{}, fmt.Errorf("loaded CNI configuration has no bridge network")
}

func (b *libCNIBackend) Setup(ctx context.Context, containerID, netnsPath string) (*types100.Result, error) {
	network := b.outerNetwork
	result, err := b.client.AddNetworkList(ctx, network.config, runtimeConf(containerID, netnsPath, network.ifName))
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
	network := b.outerNetwork
	if err := b.client.DelNetworkList(ctx, network.config, runtimeConf(containerID, netnsPath, network.ifName)); err != nil {
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
