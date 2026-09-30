package id

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		id   string
		ok   bool
	}{
		{name: "minimum length", id: "a1", ok: true},
		{name: "maximum length", id: strings.Repeat("a", MaxLength), ok: true},
		{name: "separators", id: "sandbox.V1_test-01", ok: true},
		{name: "canonical uuid", id: "a541d234-baf9-4d67-a3ca-5696e49e39db", ok: true},
		{name: "non canonical uuid", id: "A541D234-BAF9-4D67-A3CA-5696E49E39DB"},
		{name: "uuid with wrong separator", id: "a541d234_baf9_4d67_a3ca_5696e49e39db"},
		{name: "36 characters not a uuid", id: strings.Repeat("a", 36)},
		{name: "too short", id: "a"},
		{name: "too long", id: strings.Repeat("a", MaxLength+1)},
		{name: "invalid first character", id: "-sandbox"},
		{name: "command substitution", id: "x$(id)"},
		{name: "path separator", id: "x/y"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Validate(tt.id) == nil; got != tt.ok {
				t.Fatalf("Validate(%q) success = %t, want %t", tt.id, got, tt.ok)
			}
		})
	}
}

func TestNew(t *testing.T) {
	value, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if len(value) != 32 {
		t.Fatalf("New() length = %d, want 32", len(value))
	}
	if err := Validate(value); err != nil {
		t.Fatalf("New() generated invalid ID %q: %v", value, err)
	}
}

func TestNewWithPrefix(t *testing.T) {
	value, err := NewWithPrefix("wh_")
	if err != nil {
		t.Fatalf("NewWithPrefix() error = %v", err)
	}
	if len(value) != len("wh_")+32 || !strings.HasPrefix(value, "wh_") {
		t.Fatalf("NewWithPrefix() = %q, want wh_ prefix and 32 hex characters", value)
	}
}
