// Package apperror defines transport-independent application errors.
package apperror

import (
	"fmt"
	"regexp"
	"strings"
)

// Kind is a transport-independent error category.
type Kind uint8

const (
	Internal Kind = iota
	InvalidArgument
	Unauthenticated
	PermissionDenied
	NotFound
	AlreadyExists
	Conflict
	FailedPrecondition
	ResourceExhausted
	UpstreamFailure
	Unavailable
	DeadlineExceeded
	NotImplemented
)

// Code is a stable, machine-readable application error identifier.
type Code string

var codePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)

// Valid reports whether c follows the public <domain>.<reason> convention.
func (c Code) Valid() bool {
	return codePattern.MatchString(string(c))
}

// Error contains a public classification and an optional private cause.
// Its fields are intentionally immutable outside this package.
type Error struct {
	kind          Kind
	code          Code
	publicMessage string
	cause         error
}

// Define creates an immutable application error prototype. Invalid definitions
// are programmer errors and panic during package initialization.
func Define(kind Kind, code Code, publicMessage string) *Error {
	publicMessage = strings.TrimSpace(publicMessage)
	if !code.Valid() {
		panic(fmt.Sprintf("apperror: invalid code %q", code))
	}
	if publicMessage == "" {
		panic(fmt.Sprintf("apperror: empty public message for %q", code))
	}
	return &Error{kind: kind, code: code, publicMessage: publicMessage}
}

func (e *Error) Kind() Kind {
	if e == nil {
		return Internal
	}
	return e.kind
}

func (e *Error) Code() Code {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *Error) PublicMessage() string {
	if e == nil {
		return ""
	}
	return e.publicMessage
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.cause == nil {
		return e.publicMessage
	}
	return e.publicMessage + ": " + e.cause.Error()
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Is compares application errors by stable Code.
func (e *Error) Is(target error) bool {
	if e == nil {
		return false
	}
	other, ok := target.(*Error)
	return ok && other != nil && e.code != "" && e.code == other.code
}

// New returns a new instance of the prototype without a cause.
func (e *Error) New() *Error {
	if e == nil {
		return nil
	}
	return &Error{kind: e.kind, code: e.code, publicMessage: e.publicMessage}
}

// Wrap returns a new instance that retains cause in the error chain.
func (e *Error) Wrap(cause error) *Error {
	wrapped := e.New()
	if wrapped != nil {
		wrapped.cause = cause
	}
	return wrapped
}

// WrapMessage returns a new instance with an explicitly reviewed public
// message. Empty messages fall back to the prototype's message.
func (e *Error) WrapMessage(cause error, publicMessage string) *Error {
	wrapped := e.Wrap(cause)
	if wrapped == nil {
		return nil
	}
	if publicMessage = strings.TrimSpace(publicMessage); publicMessage != "" {
		wrapped.publicMessage = publicMessage
	}
	return wrapped
}
