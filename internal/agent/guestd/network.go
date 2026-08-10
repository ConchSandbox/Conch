// Copyright (c) Huawei Technologies Co., Ltd. 2026. All rights reserved.
// Description: Network configuration for conch-init PID 1

package guestd

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/openeuler/Conch/internal/netstack"
	"github.com/openeuler/Conch/pkg/ulog"
	"github.com/vishvananda/netlink"
)

var resolverTargetPath = "/etc/resolv.conf"

// applyGuestNetworkConfig configures a cold guest or validates restored state.
// Once the sandbox is ready, address and route identity are never changed.
func applyGuestNetworkConfig(cfg netstack.GuestNetworkConfig, revalidate bool) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	link, err := firstGuestLink()
	if err != nil {
		return err
	}
	if revalidate {
		if err := validateGuestLinkConfig(link, cfg); err != nil {
			return err
		}
	} else {
		if err := configureGuestLink(link, cfg); err != nil {
			return err
		}
	}
	if err := installResolverConfig(cfg.DNS); err != nil {
		return fmt.Errorf("install resolver config: %w", err)
	}
	return nil
}

func firstGuestLink() (netlink.Link, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("list network interfaces: %w", err)
	}
	for _, link := range links {
		attrs := link.Attrs()
		if attrs == nil || attrs.Name == "" || attrs.Flags&net.FlagLoopback != 0 {
			continue
		}
		return link, nil
	}
	return nil, fmt.Errorf("no non-loopback network interface found")
}

func configureGuestLink(link netlink.Link, cfg netstack.GuestNetworkConfig) error {
	if err := setLinkUpByName("lo"); err != nil {
		return fmt.Errorf("bring loopback up: %w", err)
	}
	cidr := fmt.Sprintf("%s/%d", cfg.GuestIP, cfg.PrefixLength)
	if err := ensureAddress(link, cidr); err != nil {
		return fmt.Errorf("assign guest address: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring guest interface up: %w", err)
	}
	if err := ensureDefaultRoute(link, cfg.Gateway); err != nil {
		return fmt.Errorf("set default route: %w", err)
	}
	ulog.GetLogger().Info("Guest network configured", ulog.F("name", link.Attrs().Name), ulog.F("address", cidr), ulog.F("gateway", cfg.Gateway))
	return nil
}

func validateGuestLinkConfig(link netlink.Link, cfg netstack.GuestNetworkConfig) error {
	attrs := link.Attrs()
	if attrs == nil || attrs.Flags&net.FlagUp == 0 {
		return fmt.Errorf("guest interface is not up")
	}
	cidr := fmt.Sprintf("%s/%d", cfg.GuestIP, cfg.PrefixLength)
	_, expected, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}
	expected.IP = net.ParseIP(cfg.GuestIP)
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list guest addresses: %w", err)
	}
	found := false
	for _, addr := range addrs {
		if addr.IPNet != nil && addr.IPNet.IP.Equal(expected.IP) && maskEqual(addr.IPNet.Mask, expected.Mask) {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("guest interface does not have required address %s", cidr)
	}
	gw := net.ParseIP(cfg.Gateway)
	if !routeExists(link, nil, gw) {
		return fmt.Errorf("guest interface does not have required default gateway %s", cfg.Gateway)
	}
	return nil
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
	if isDefaultRouteDst(a) || isDefaultRouteDst(b) {
		return isDefaultRouteDst(a) && isDefaultRouteDst(b)
	}
	return a.IP.Equal(b.IP) && maskEqual(a.Mask, b.Mask)
}

func isDefaultRouteDst(dst *net.IPNet) bool {
	if dst == nil {
		return true
	}
	ones, bits := dst.Mask.Size()
	return bits == net.IPv4len*8 && ones == 0 && dst.IP.To4() != nil && dst.IP.To4().Equal(net.IPv4zero)
}

func installResolverConfig(cfg netstack.DNSConfig) error {
	normalized, err := netstack.NormalizeDNS(cfg)
	if err != nil {
		return err
	}
	cfg = normalized
	var out bytes.Buffer
	out.WriteString("# Generated by Conch\n")
	for _, server := range cfg.Nameservers {
		fmt.Fprintf(&out, "nameserver %s\n", server)
	}
	if len(cfg.Search) != 0 {
		fmt.Fprintf(&out, "search %s\n", strings.Join(cfg.Search, " "))
	} else if cfg.Domain != "" {
		fmt.Fprintf(&out, "domain %s\n", cfg.Domain)
	}
	if len(cfg.Options) != 0 {
		fmt.Fprintf(&out, "options %s\n", strings.Join(cfg.Options, " "))
	}
	if err := os.Remove(resolverTargetPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove existing resolver config: %w", err)
	}
	return os.WriteFile(resolverTargetPath, out.Bytes(), 0o644)
}
