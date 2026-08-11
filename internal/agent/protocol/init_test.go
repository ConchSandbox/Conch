package protocol

import (
	"errors"
	"testing"
)

func TestValidateEnvironment(t *testing.T) {
	for _, test := range []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{name: "nil", ok: true},
		{name: "empty map", env: map[string]string{}, ok: true},
		{name: "valid", env: map[string]string{"EMPTY": "", "WITH_EQUALS": "a=b"}, ok: true},
		{name: "empty key", env: map[string]string{"": "value"}},
		{name: "equals in key", env: map[string]string{"BAD=KEY": "value"}},
		{name: "NUL in key", env: map[string]string{"BAD\x00KEY": "value"}},
		{name: "NUL in value", env: map[string]string{"KEY": "bad\x00value"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateEnvironment(test.env)
			if test.ok && err != nil {
				t.Fatalf("ValidateEnvironment() error = %v", err)
			}
			if !test.ok && !errors.Is(err, ErrInvalidEnvironment) {
				t.Fatalf("ValidateEnvironment() error = %v, want ErrInvalidEnvironment", err)
			}
		})
	}
}
