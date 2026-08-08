package netstack

import (
	"testing"
)

func TestGuestNetworkConfigValidate(t *testing.T) {
	valid := GuestNetworkConfig{
		GuestIP:      "192.168.100.21",
		PrefixLength: 24,
		Gateway:      "192.168.100.2",
		DNS:          DNSConfig{Nameservers: []string{"10.0.0.53"}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	for _, tt := range []struct {
		name   string
		mutate func(*GuestNetworkConfig)
	}{
		{name: "IPv6 guest", mutate: func(c *GuestNetworkConfig) { c.GuestIP = "fd00::2" }},
		{name: "invalid prefix", mutate: func(c *GuestNetworkConfig) { c.PrefixLength = 33 }},
		{name: "gateway outside subnet", mutate: func(c *GuestNetworkConfig) { c.Gateway = "192.168.101.2" }},
		{name: "same address", mutate: func(c *GuestNetworkConfig) { c.Gateway = c.GuestIP }},
		{name: "loopback DNS", mutate: func(c *GuestNetworkConfig) { c.DNS.Nameservers = []string{"127.0.0.53"} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid.Clone()
			tt.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
		})
	}
}
