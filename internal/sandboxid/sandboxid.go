package sandboxid

import (
	"fmt"
	"regexp"
)

const (
	MinLength = 2
	MaxLength = 32
	chars     = `[a-zA-Z0-9][a-zA-Z0-9_.-]`
)

var pattern = regexp.MustCompile(`^` + chars + `+$`)

func Validate(id string) error {
	if len(id) < MinLength || len(id) > MaxLength {
		return fmt.Errorf("length must be between %d and %d characters", MinLength, MaxLength)
	}
	if !pattern.MatchString(id) {
		return fmt.Errorf("only %s are allowed", chars)
	}
	return nil
}
