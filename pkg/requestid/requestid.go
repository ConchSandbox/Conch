// Package requestid carries correlation IDs independently of cancellation,
// sandbox identity, idempotency keys, and tracing.
package requestid

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

const Header = "X-Request-ID"
const MaxLength = 128

type contextKey struct{}

// Valid accepts 1-128 ASCII letters, digits, dots, underscores, or hyphens.
func Valid(id string) bool {
	if len(id) == 0 || len(id) > MaxLength {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// Ensure preserves valid IDs and replaces missing or invalid IDs with a UUID.
func Ensure(id string) string {
	if Valid(id) {
		return id
	}
	return uuid.NewString()
}

// With stores a validated ID using a private context key. Empty IDs allow
// internal work without a source request to remain uncorrelated.
func With(ctx context.Context, id string) context.Context {
	if !Valid(id) {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, id)
}

func FromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}

// Middleware applies the same contract to all responses, including mux errors
// and streaming RPCs. Ambiguous multi-value headers are treated as invalid.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := ""
		if values := r.Header.Values(Header); len(values) == 1 {
			id = values[0]
		}
		id = Ensure(id)
		w.Header().Set(Header, id)
		next.ServeHTTP(w, r.WithContext(With(r.Context(), id)))
	})
}
