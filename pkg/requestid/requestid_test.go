package requestid

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestMiddlewareRequestIDs(t *testing.T) {
	for _, tt := range []struct {
		name     string
		values   []string
		preserve bool
	}{
		{"missing", nil, false},
		{"valid", []string{"client.A_1-2"}, true},
		{"max length", []string{strings.Repeat("a", MaxLength)}, true},
		{"empty", []string{""}, false},
		{"too long", []string{strings.Repeat("a", MaxLength+1)}, false},
		{"newline", []string{"bad\nforged=entry"}, false},
		{"whitespace", []string{" id "}, false},
		{"unicode", []string{"请求"}, false},
		{"comma", []string{"one,two"}, false},
		{"multiple", []string{"one", "two"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusInternalServerError} {
				r := httptest.NewRequest(http.MethodGet, "/", nil)
				for _, value := range tt.values {
					r.Header.Add(Header, value)
				}
				w := httptest.NewRecorder()
				Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if id := FromContext(r.Context()); !Valid(id) || id != w.Header().Get(Header) {
						t.Fatalf("context and response ID mismatch: %q, %q", id, w.Header().Get(Header))
					}
					w.WriteHeader(status)
				})).ServeHTTP(w, r)
				id := w.Header().Get(Header)
				if w.Code != status || !Valid(id) {
					t.Fatalf("response = %d, ID %q", w.Code, id)
				}
				if tt.preserve && id != tt.values[0] {
					t.Fatalf("ID = %q, want %q", id, tt.values[0])
				}
				if !tt.preserve && len(tt.values) != 0 && id == tt.values[0] {
					t.Fatal("invalid ID was echoed")
				}
			}
		})
	}
}

func TestConcurrentRequestsKeepSeparateIDs(t *testing.T) {
	var wg sync.WaitGroup
	ids := make(chan string, 100)
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := FromContext(r.Context())
		if want := r.Header.Get(Header); want != "" && id != want {
			t.Errorf("context ID = %q, want %q", id, want)
		}
		if id != w.Header().Get(Header) {
			t.Error("response and context IDs differ")
		}
	}))
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if i%2 == 0 {
				r.Header.Set(Header, fmt.Sprintf("request-%d", i))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			ids <- w.Header().Get(Header)
		}()
	}
	wg.Wait()
	close(ids)
	seen := make(map[string]bool)
	for id := range ids {
		if !Valid(id) || seen[id] {
			t.Fatalf("invalid or duplicated ID %q", id)
		}
		seen[id] = true
	}
}

func TestContextUsesPrivateValidatedKey(t *testing.T) {
	ctx := context.WithValue(context.Background(), "request_id", "untrusted\nvalue")
	if FromContext(ctx) != "" {
		t.Fatal("string context key was accepted")
	}
	if FromContext(With(ctx, "invalid value")) != "" {
		t.Fatal("invalid ID was accepted")
	}
	ctx = With(ctx, "origin-request")
	if FromContext(context.WithoutCancel(ctx)) != "origin-request" {
		t.Fatal("detached context lost source ID")
	}
}
