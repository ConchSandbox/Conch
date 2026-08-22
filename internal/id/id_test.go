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
