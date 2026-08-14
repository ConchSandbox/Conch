package sandboxid

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
