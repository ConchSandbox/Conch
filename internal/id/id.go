// Package id provides generation and validation for Conch identifiers.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
)

const (
	MinLength = 2
	MaxLength = 32
	chars     = `[a-zA-Z0-9][a-zA-Z0-9_.-]`
)

var pattern = regexp.MustCompile(`^` + chars + `+$`)

func New() (string, error) { return NewWithPrefix("") }

func NewWithPrefix(prefix string) (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + hex.EncodeToString(data[:]), nil
}

func Validate(value string) error {
	if len(value) < MinLength || len(value) > MaxLength {
		return fmt.Errorf("length must be between %d and %d characters", MinLength, MaxLength)
	}
	if !pattern.MatchString(value) {
		return fmt.Errorf("only %s are allowed", chars)
	}
	return nil
}
