package apperror

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestDefineRejectsInvalidDefinitions(t *testing.T) {
	tests := []struct {
		name    string
		code    Code
		message string
	}{
		{name: "empty code", message: "message"},
		{name: "missing domain", code: "reason", message: "message"},
		{name: "uppercase", code: "Sandbox.reason", message: "message"},
		{name: "empty message", code: "sandbox.reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Define() did not panic")
				}
			}()
			_ = Define(InvalidArgument, tt.code, tt.message)
		})
	}
}

func TestWrapPreservesClassificationAndCause(t *testing.T) {
	prototype := Define(NotFound, "sandbox.not_found", "sandbox not found")
	cause := errors.New("database path and internal detail")
	err := fmt.Errorf("get sandbox: %w", prototype.Wrap(cause))

	var appErr *Error
	if !errors.As(err, &appErr) {
		t.Fatal("errors.As() did not find application error")
	}
	if appErr.Kind() != NotFound || appErr.Code() != "sandbox.not_found" || appErr.PublicMessage() != "sandbox not found" {
		t.Fatalf("application error = %#v", appErr)
	}
	if !errors.Is(err, prototype) || !errors.Is(err, cause) {
		t.Fatalf("error chain does not retain prototype and cause: %v", err)
	}
}

func TestInstancesDoNotMutatePrototype(t *testing.T) {
	prototype := Define(InvalidArgument, "request.invalid_body", "invalid request body")
	first := prototype.WrapMessage(errors.New("first"), "reviewed message")
	second := prototype.Wrap(nil)

	if first == prototype || second == prototype || first == second {
		t.Fatal("constructors returned shared instances")
	}
	if first.PublicMessage() != "reviewed message" {
		t.Fatalf("first message = %q", first.PublicMessage())
	}
	if second.PublicMessage() != "invalid request body" || prototype.PublicMessage() != "invalid request body" {
		t.Fatal("prototype was mutated")
	}
	if second.Unwrap() != nil {
		t.Fatalf("Wrap(nil) cause = %v", second.Unwrap())
	}
	if got := prototype.WrapMessage(errors.New("empty"), "   ").PublicMessage(); got != prototype.PublicMessage() {
		t.Fatalf("empty WrapMessage() message = %q, want %q", got, prototype.PublicMessage())
	}
}

func TestPrototypeCanBeWrappedConcurrently(t *testing.T) {
	prototype := Define(Conflict, "sandbox.concurrent_update", "sandbox update conflicted")
	const workers = 64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			wrapped := prototype.WrapMessage(fmt.Errorf("cause %d", i), fmt.Sprintf("message %d", i))
			if wrapped.Code() != prototype.Code() || wrapped.PublicMessage() == prototype.PublicMessage() {
				t.Errorf("wrapped error = %#v", wrapped)
			}
		}(i)
	}
	wg.Wait()
	if prototype.PublicMessage() != "sandbox update conflicted" || prototype.Unwrap() != nil {
		t.Fatalf("prototype was mutated: %#v", prototype)
	}
}
