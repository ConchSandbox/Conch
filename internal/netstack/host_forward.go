package netstack

import (
	"errors"
	"fmt"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
)

const hostForwardTable = "filter"

func defaultGatewayInterface() (string, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return "", fmt.Errorf("error fetching routes: %w", err)
	}
	for _, route := range routes {
		if route.Dst == nil || route.Dst.String() != "0.0.0.0/0" || route.Gw == nil {
			continue
		}
		link, err := netlink.LinkByIndex(route.LinkIndex)
		if err != nil {
			return "", fmt.Errorf("error fetching interface for default gateway: %w", err)
		}
		return link.Attrs().Name, nil
	}
	return "", errors.New("cannot find default gateway")
}

func hostForwardRules(bridgeName, gatewayInterface string) [][]string {
	return [][]string{
		{"-i", bridgeName, "-o", gatewayInterface, "-j", "ACCEPT"},
		{"-i", gatewayInterface, "-o", bridgeName, "-j", "ACCEPT"},
	}
}

func ensureHostForwardingRules(bridgeName, gatewayInterface string) error {
	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("initialize host iptables: %w", err)
	}
	for _, rule := range hostForwardRules(bridgeName, gatewayInterface) {
		exists, err := tables.Exists(hostForwardTable, "FORWARD", rule...)
		if err != nil {
			return fmt.Errorf("check host FORWARD rule %v: %w", rule, err)
		}
		if exists {
			continue
		}
		if err := tables.Insert(hostForwardTable, "FORWARD", 1, rule...); err != nil {
			return fmt.Errorf("insert host FORWARD rule %v: %w", rule, err)
		}
	}
	return nil
}

func removeHostForwardingRules(bridgeName, gatewayInterface string) error {
	tables, err := iptables.New()
	if err != nil {
		return fmt.Errorf("initialize host iptables: %w", err)
	}
	var errs []error
	for _, rule := range hostForwardRules(bridgeName, gatewayInterface) {
		if err := tables.DeleteIfExists(hostForwardTable, "FORWARD", rule...); err != nil {
			errs = append(errs, fmt.Errorf("delete host FORWARD rule %v: %w", rule, err))
		}
	}
	return errors.Join(errs...)
}
