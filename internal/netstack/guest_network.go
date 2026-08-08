package netstack

import (
	"fmt"
	"net"
	"strings"
)

type DNSConfig struct {
	Nameservers []string `json:"nameservers,omitempty"`
	Domain      string   `json:"domain,omitempty"`
	Search      []string `json:"search,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type GuestNetworkConfig struct {
	GuestIP      string    `json:"guestIP"`
	PrefixLength int       `json:"prefixLength"`
	Gateway      string    `json:"gateway"`
	DNS          DNSConfig `json:"dns"`
}

func (c DNSConfig) Clone() DNSConfig {
	return DNSConfig{
		Nameservers: append([]string(nil), c.Nameservers...),
		Domain:      c.Domain,
		Search:      append([]string(nil), c.Search...),
		Options:     append([]string(nil), c.Options...),
	}
}

func (c GuestNetworkConfig) Clone() GuestNetworkConfig {
	c.DNS = c.DNS.Clone()
	return c
}

func (c GuestNetworkConfig) Validate() error {
	ip := net.ParseIP(strings.TrimSpace(c.GuestIP))
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("guestIP %q must be a valid IPv4 address", c.GuestIP)
	}
	gw := net.ParseIP(strings.TrimSpace(c.Gateway))
	if gw == nil || gw.To4() == nil {
		return fmt.Errorf("gateway %q must be a valid IPv4 address", c.Gateway)
	}
	if c.PrefixLength < 1 || c.PrefixLength > 32 {
		return fmt.Errorf("prefixLength %d must be within [1, 32]", c.PrefixLength)
	}
	mask := net.CIDRMask(c.PrefixLength, 32)
	if !ip.Mask(mask).Equal(gw.Mask(mask)) {
		return fmt.Errorf("guestIP %s/%d and gateway %s are not in the same subnet", ip, c.PrefixLength, gw)
	}
	if ip.Equal(gw) {
		return fmt.Errorf("guestIP and gateway must be different")
	}
	if _, err := NormalizeDNS(c.DNS); err != nil {
		return fmt.Errorf("invalid DNS config: %w", err)
	}
	return nil
}
