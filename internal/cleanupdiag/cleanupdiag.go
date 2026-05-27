package cleanupdiag

import (
	"sync"
	"time"

	"github.com/openeuler/Conch/pkg/ulog"
)

const slowCleanupThreshold = 5 * time.Second

// Start records cleanup progress and emits a warning if a cleanup stage stays
// running long enough to explain a Ctrl+C hang.
func Start(stage string, fields ...ulog.Field) func(error) {
	logger := ulog.GetLogger()
	start := time.Now()
	stageFields := appendFields(fields, ulog.F("stage", stage))
	logger.Info("cleanup stage started", stageFields...)

	timer := time.AfterFunc(slowCleanupThreshold, func() {
		logger.Warn("cleanup stage still running",
			appendFields(stageFields, ulog.F("elapsed", time.Since(start).String()))...,
		)
	})

	var once sync.Once
	return func(err error) {
		once.Do(func() {
			timer.Stop()
			elapsedFields := appendFields(stageFields, ulog.F("elapsed", time.Since(start).String()))
			if err != nil {
				logger.Error("cleanup stage failed", appendFields(elapsedFields, ulog.F("error", err))...)
				return
			}
			logger.Info("cleanup stage completed", elapsedFields...)
		})
	}
}

func appendFields(fields []ulog.Field, extra ...ulog.Field) []ulog.Field {
	out := make([]ulog.Field, 0, len(fields)+len(extra))
	out = append(out, fields...)
	out = append(out, extra...)
	return out
}
