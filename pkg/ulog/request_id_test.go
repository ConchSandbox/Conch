package ulog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/openeuler/Conch/pkg/requestid"
)

func TestContextLoggersKeepConcurrentRequestIDsSeparate(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "requests.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	base := (&ulog{level: InfoLevel, writer: &writeSyncer{file: f}}).With(F("sandboxId", "same-sandbox"))
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("request-%d", i)
			logger := base.WithContext(requestid.With(context.Background(), id))
			logger.Info("operation", F("expected_id", id), F("sandbox_id", "same-sandbox"))
		}()
	}
	wg.Wait()
	base.Info("background maintenance")
	base.WithContext(requestid.With(context.Background(), "first")).WithContext(requestid.With(context.Background(), "second")).Info("rebound")
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.Contains(line, "[same-sandbox]") {
			t.Fatalf("legacy sandbox prefix missing: %s", line)
		}
		if strings.Contains(line, "background maintenance") {
			if strings.Contains(line, "request_id=") {
				t.Fatalf("request ID leaked to base logger: %s", line)
			}
			continue
		}
		if strings.Count(line, "request_id=") != 1 {
			t.Fatalf("ambiguous log: %s", line)
		}
		if strings.Contains(line, "rebound") {
			if !strings.Contains(line, "request_id=second") {
				t.Fatal(line)
			}
			continue
		}
		var actual, expected string
		for _, field := range strings.Fields(line) {
			if value, ok := strings.CutPrefix(field, "request_id="); ok {
				actual = value
			}
			if value, ok := strings.CutPrefix(field, "expected_id="); ok {
				expected = value
			}
		}
		if actual != expected {
			t.Fatalf("requests mixed in log: %s", line)
		}
	}
}

func TestWithContextPreservesFieldsWithoutMutatingBase(t *testing.T) {
	base := &ulog{
		level: InfoLevel, writer: &requestIDBenchWriter{}, sandboxId: "sandbox-original",
		// Spare capacity catches accidental appends to the base field backing array.
		fields: make([]Field, 1, 8),
	}
	base.fields[0] = F("component", "api")
	ctx := requestid.With(context.Background(), "first-request")
	ctx = context.WithValue(ctx, "user_id", "user-1")
	ctx = context.WithValue(ctx, "trace_id", "trace-1")
	derived := base.WithContext(ctx).(*ulog)
	if derived.writer != base.writer || derived.sandboxId != base.sandboxId {
		t.Fatal("context logger changed writer or sandbox prefix")
	}
	if len(base.fields) != 1 {
		t.Fatal("context fields leaked into base logger")
	}
	for _, field := range base.fields[1:cap(base.fields)] {
		if field.Key != "" || field.Value != nil {
			t.Fatal("context logger mutated base field storage")
		}
	}
	check := func(logger *ulog, requestID string) {
		t.Helper()
		want := map[string]string{"component": "api", "user_id": "user-1", "trace_id": "trace-1", "request_id": requestID}
		if len(logger.fields) != len(want) {
			t.Fatalf("fields = %#v", logger.fields)
		}
		for _, field := range logger.fields {
			if field.Value != want[field.Key] {
				t.Fatalf("unexpected field %#v", field)
			}
			delete(want, field.Key)
		}
		if len(want) != 0 {
			t.Fatalf("missing fields: %#v", want)
		}
	}
	check(derived, "first-request")
	rebound := derived.WithContext(requestid.With(context.Background(), "second-request")).(*ulog)
	check(rebound, "second-request")
	check(derived, "first-request")
}

func TestWithContextReusesLoggerWhenFieldsAreUnchanged(t *testing.T) {
	base := &ulog{level: InfoLevel, writer: &requestIDBenchWriter{}}
	for _, ctx := range []context.Context{nil, context.Background()} {
		if base.WithContext(ctx) != base {
			t.Fatal("empty context unnecessarily cloned the logger")
		}
	}
	ctx := requestid.With(context.Background(), "same-request")
	derived := base.WithContext(ctx)
	if derived.WithContext(ctx) != derived {
		t.Fatal("unchanged request ID unnecessarily cloned the logger")
	}
	if allocs := testing.AllocsPerRun(100, func() { requestIDBenchLogger = derived.WithContext(ctx) }); allocs != 0 {
		t.Fatalf("unchanged context allocated %v objects", allocs)
	}
}
