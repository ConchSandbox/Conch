// Package apierror defines the stable public errors returned by the control-plane API.
package apierror

import "fmt"

const (
	EnvelopeVersion     = 1
	CodeInvalidArgument = "invalid_argument"
	CodeNotFound        = "not_found"
	CodeInternalError   = "internal_error"
	ResourceTemplate    = "template"
)

// Envelope contains only client-safe error details.
type Envelope struct {
	Version      int    `json:"version"`
	Code         string `json:"code"`
	ResourceType string `json:"resource_type,omitempty"`
	Message      string `json:"message"`
	RequestID    string `json:"request_id"`
}

// Error is a decoded control-plane error.
type Error struct {
	StatusCode int
	Envelope
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("control-plane request failed with HTTP %d", e.StatusCode)
}

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok || t == nil {
		return false
	}
	if t.Code != "" && e.Code != t.Code {
		return false
	}
	if t.ResourceType != "" && e.ResourceType != t.ResourceType {
		return false
	}
	return t.Code != "" || t.ResourceType != ""
}

// ErrTemplateNotFound supports errors.Is checks without message matching.
var ErrTemplateNotFound = &Error{Envelope: Envelope{Code: CodeNotFound, ResourceType: ResourceTemplate}}
