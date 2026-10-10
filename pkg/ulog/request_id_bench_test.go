package ulog

import (
	"context"
	"testing"

	"github.com/openeuler/Conch/pkg/requestid"
)

// Exclude output I/O so the benchmark measures logger and field overhead.
type requestIDBenchWriter struct{}

func (*requestIDBenchWriter) Write(p []byte) (int, error) { return len(p), nil }
func (*requestIDBenchWriter) Sync() error                 { return nil }

var requestIDBenchLogger Logger

func BenchmarkRequestIDLogOverhead(b *testing.B) {
	base := (&ulog{level: InfoLevel, writer: &requestIDBenchWriter{}}).With(F("sandboxId", "sandbox-bench"))
	ctx := requestid.With(context.Background(), "request-bench-206")
	b.Run("ConstructWithContext", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			requestIDBenchLogger = base.WithContext(ctx)
		}
	})
	b.Run("InfoWithoutContext", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			base.Info("request handled")
		}
	})
	b.Run("RebindSameContext", func(b *testing.B) {
		logger := base.WithContext(ctx)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			requestIDBenchLogger = logger.WithContext(ctx)
		}
	})
	b.Run("InfoWithContextEveryCall", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			base.WithContext(ctx).Info("request handled")
		}
	})
	b.Run("InfoWithReusedContextLogger", func(b *testing.B) {
		logger := base.WithContext(ctx)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			logger.Info("request handled")
		}
	})
}
