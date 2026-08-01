/*
Copyright the Conch Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package netstack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openeuler/Conch/pkg/ulog"
	"github.com/vishvananda/netlink"
)

func deleteEmptyCNIHostBridge(ctx context.Context, bridgeName string, retries int, delay time.Duration) error {
	if bridgeName == "" {
		return nil
	}
	for attempt := 0; attempt <= retries; attempt++ {
		link, err := netlink.LinkByName(bridgeName)
		if isLinkNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("finding cni bridge %s: %w", bridgeName, err)
		}
		if _, ok := link.(*netlink.Bridge); !ok {
			getLogger().Warn("skipping cni host artifact cleanup for non-bridge link", ulog.F("bridge", bridgeName), ulog.F("type", link.Type()))
			return nil
		}

		ports, err := bridgePorts(link.Attrs().Index)
		if err != nil {
			return fmt.Errorf("checking cni bridge %s ports: %w", bridgeName, err)
		}
		if len(ports) == 0 {
			if err := netlink.LinkDel(link); err != nil {
				if isLinkNotFound(err) {
					return nil
				}
				return fmt.Errorf("deleting cni bridge %s: %w", bridgeName, err)
			}
			getLogger().Info("deleted cni host bridge", ulog.F("bridge", bridgeName))
			return nil
		}
		if attempt == retries {
			return fmt.Errorf("cni bridge %s still has enslaved interfaces: %s", bridgeName, strings.Join(ports, ","))
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), fmt.Errorf("waiting for cni bridge %s to become empty", bridgeName))
		case <-time.After(delay):
		}
	}
	return nil
}

func bridgePorts(masterIndex int) ([]string, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	ports := make([]string, 0)
	for _, link := range links {
		attrs := link.Attrs()
		if attrs != nil && attrs.MasterIndex == masterIndex {
			ports = append(ports, attrs.Name)
		}
	}
	return ports, nil
}

func isLinkNotFound(err error) bool {
	if err == nil {
		return false
	}
	var linkNotFound netlink.LinkNotFoundError
	return errors.As(err, &linkNotFound) || os.IsNotExist(err)
}
