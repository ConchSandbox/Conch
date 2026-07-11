package netstack

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSandboxNetworkConfigJSONSchema(t *testing.T) {
	raw := []byte(`{
		"allowPublicTraffic": true,
		"allowOut": ["10.0.0.0/8", "example.com"],
		"denyOut": ["192.0.2.1"],
		"egressProxy": "http://proxy.internal:8080",
		"maskRequestHost": false,
		"allow_internet_access": true,
		"rules": [{"type": "domain", "name": "example.com"}]
	}`)

	var cfg SandboxNetworkConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if cfg.AllowPublicTraffic == nil || !*cfg.AllowPublicTraffic {
		t.Fatalf("AllowPublicTraffic = %v, want true", cfg.AllowPublicTraffic)
	}
	if got := string(cfg.AllowOut[1]); got != "example.com" {
		t.Fatalf("AllowOut[1] = %q, want example.com", got)
	}
	if cfg.MaskRequestHost == nil || *cfg.MaskRequestHost {
		t.Fatalf("MaskRequestHost = %v, want false", cfg.MaskRequestHost)
	}
	if got := cfg.Rules[0]["name"]; got != "example.com" {
		t.Fatalf("Rules[0][name] = %v, want example.com", got)
	}
	if cfg.AllowInternetAccess == nil || !*cfg.AllowInternetAccess {
		t.Fatalf("AllowInternetAccess = %v, want true", cfg.AllowInternetAccess)
	}
}

func TestSandboxNetworkUpdateConfigJSONSchema(t *testing.T) {
	raw := []byte(`{
		"allowOut": ["203.0.113.0/24"],
		"denyOut": ["bad.example"],
		"egressProxy": "http://proxy.internal:8080",
		"rules": [{"type": "domain"}],
		"allow_internet_access": true
	}`)

	var cfg SandboxNetworkUpdateConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if cfg.AllowInternetAccess == nil || !*cfg.AllowInternetAccess {
		t.Fatalf("AllowInternetAccess = %v, want true", cfg.AllowInternetAccess)
	}
	if cfg.DenyOut == nil {
		t.Fatalf("DenyOut = nil, want bad.example")
	}
	if got := string((*cfg.DenyOut)[0]); got != "bad.example" {
		t.Fatalf("DenyOut[0] = %q, want bad.example", got)
	}
}

func TestNormalizePolicyDestination(t *testing.T) {
	tests := []struct {
		raw string
		ok  bool
	}{
		{raw: "10.0.0.1", ok: true},
		{raw: "10.0.0.0/8", ok: true},
		{raw: "example.com", ok: false},
		{raw: "", ok: false},
	}
	for _, tt := range tests {
		_, ok := normalizePolicyDestination(tt.raw)
		if ok != tt.ok {
			t.Fatalf("normalizePolicyDestination(%q) ok = %v, want %v", tt.raw, ok, tt.ok)
		}
	}
}

func TestValidateSandboxNetworkPolicy(t *testing.T) {
	ctx := context.Background()
	allowPublic := true
	maskHost := true
	proxy := "http://proxy.internal:8080"
	if err := ValidateSandboxNetworkPolicy(ctx, &SandboxNetworkConfig{
		AllowPublicTraffic: &allowPublic,
		AllowOut:           []SandboxNetworkAddress{"10.0.0.1", "10.0.0.0/8"},
		DenyOut:            []SandboxNetworkAddress{"192.0.2.1"},
		EgressProxy:        &proxy,
		MaskRequestHost:    &maskHost,
		Rules:              []SandboxNetworkRule{{"type": "domain"}},
	}); err != nil {
		t.Fatalf("ValidateSandboxNetworkPolicy() error = %v", err)
	}

	for _, tt := range []struct {
		name  string
		cfg   *SandboxNetworkConfig
		field string
	}{
		{
			name:  "empty allow",
			cfg:   &SandboxNetworkConfig{AllowOut: []SandboxNetworkAddress{""}},
			field: "allowOut",
		},
		{
			name:  "invalid deny",
			cfg:   &SandboxNetworkConfig{DenyOut: []SandboxNetworkAddress{"%%%"}},
			field: "denyOut",
		},
		{
			name:  "domain allow",
			cfg:   &SandboxNetworkConfig{AllowOut: []SandboxNetworkAddress{"example.com"}},
			field: "allowOut",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSandboxNetworkPolicy(ctx, tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("ValidateSandboxNetworkPolicy() error = %v, want field %s", err, tt.field)
			}
			if !errors.Is(err, ErrInvalidSandboxNetworkPolicy) {
				t.Fatalf("ValidateSandboxNetworkPolicy() error = %v, want ErrInvalidSandboxNetworkPolicy", err)
			}
		})
	}
}
