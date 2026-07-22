// Copyright (c) Huawei Technologies Co., Ltd. 2026. All rights reserved.
// Description: Network configuration for conch-init PID 1

package guestd

import (
	"errors"
	"net"
	"syscall"

	"github.com/openeuler/Conch/pkg/ulog"
	"github.com/vishvananda/netlink"
)

const (
	guestAddressCIDR = "192.168.100.21/24"
	defaultGateway   = "192.168.100.2"
	mmdsRouteCIDR    = "169.254.169.254/32"
)

// setupNetwork brings up the loopback and first non-lo interface with a static IP.
func setupNetwork() {
	logger := ulog.GetLogger()

	if err := setLinkUpByName("lo"); err != nil {
		logger.Warn("Failed to bring loopback up", ulog.F("error", err))
	}

	links, err := netlink.LinkList()
	if err != nil {
		logger.Error("Failed to get network interfaces", ulog.F("error", err))
		return
	}

	for _, link := range links {
		attrs := link.Attrs()
		if attrs == nil || attrs.Name == "" || attrs.Flags&net.FlagLoopback != 0 {
			continue
		}
		nicName := attrs.Name
		logger.Info("Configuring interface", ulog.F("name", nicName))

		if err := ensureAddress(link, guestAddressCIDR); err != nil {
			logger.Warn("Failed to assign address", ulog.F("name", nicName), ulog.F("error", err))
		}
		if err := netlink.LinkSetUp(link); err != nil {
			logger.Warn("Failed to bring interface up", ulog.F("name", nicName), ulog.F("error", err))
		}
		if err := ensureDefaultRoute(link, defaultGateway); err != nil {
			logger.Warn("Failed to add default route", ulog.F("name", nicName), ulog.F("error", err))
		}
		if err := ensureLinkRoute(link, mmdsRouteCIDR); err != nil {
			logger.Warn("Failed to add MMDS route", ulog.F("name", nicName), ulog.F("error", err))
		}

		logger.Info("Network configured", ulog.F("name", nicName))
		return
	}
	logger.Warn("No non-loopback network interface found")
}

func setLinkUpByName(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	return netlink.LinkSetUp(link)
}

func ensureAddress(link netlink.Link, cidr string) error {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}
	ipNet.IP = ip

	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, addr := range addrs {
		if addr.IPNet != nil && addr.IPNet.IP.Equal(ipNet.IP) && maskEqual(addr.IPNet.Mask, ipNet.Mask) {
			return nil
		}
	}

	err = netlink.AddrAdd(link, &netlink.Addr{IPNet: ipNet})
	if err == nil || errors.Is(err, syscall.EEXIST) {
		return nil
	}
	return err
}

func ensureDefaultRoute(link netlink.Link, gateway string) error {
	gw := net.ParseIP(gateway)
	if gw == nil {
		return &net.ParseError{Type: "IP address", Text: gateway}
	}
	if routeExists(link, nil, gw) {
		return nil
	}

	err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Gw:        gw,
	})
	if err == nil || errors.Is(err, syscall.EEXIST) && routeExists(link, nil, gw) {
		return nil
	}
	return err
}

func ensureLinkRoute(link netlink.Link, cidr string) error {
	_, dst, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}
	if routeExists(link, dst, nil) {
		return nil
	}

	err = netlink.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       dst,
		Scope:     netlink.SCOPE_LINK,
	})
	if err == nil || errors.Is(err, syscall.EEXIST) && routeExists(link, dst, nil) {
		return nil
	}
	return err
}

func maskEqual(a, b net.IPMask) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func routeExists(link netlink.Link, dst *net.IPNet, gw net.IP) bool {
	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return false
	}

	for _, route := range routes {
		if route.LinkIndex != link.Attrs().Index {
			continue
		}
		if !routeDstEqual(route.Dst, dst) {
			continue
		}
		if gw != nil && !route.Gw.Equal(gw) {
			continue
		}
		if gw == nil && route.Gw != nil {
			continue
		}
		return true
	}
	return false
}

func routeDstEqual(a, b *net.IPNet) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.IP.Equal(b.IP) && maskEqual(a.Mask, b.Mask)
}
