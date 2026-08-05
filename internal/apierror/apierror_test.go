package apierror

import (
	"errors"
	"testing"
)

func TestTemplateNotFoundMatchesCodeAndResource(t *testing.T) {
	templateError := &Error{Envelope: Envelope{Code: CodeNotFound, ResourceType: ResourceTemplate}}
	if !errors.Is(templateError, ErrTemplateNotFound) {
		t.Fatal("template not-found error did not match Template sentinel")
	}
	sandboxError := &Error{Envelope: Envelope{Code: CodeNotFound, ResourceType: "sandbox"}}
	if errors.Is(sandboxError, ErrTemplateNotFound) {
		t.Fatal("sandbox not-found error matched Template sentinel")
	}
}
