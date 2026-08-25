// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"log/slog"
	"sync"
	"time"
)

// boundedLogger rate-limits identical warnings so a flapping exporter cannot
// recurse into unbounded logging, and implements the OTel error-handler
// contract. Telemetry failures must never flood or crash the control plane
// (ADR-0018).
type boundedLogger struct {
	inner   *slog.Logger
	minimum time.Duration

	mu   sync.Mutex
	last map[string]time.Time
}

const defaultWarnInterval = 10 * time.Second

func newBoundedLogger(inner *slog.Logger) *boundedLogger {
	return &boundedLogger{inner: inner, minimum: defaultWarnInterval, last: make(map[string]time.Time)}
}

// Warn emits msg at most once per interval; repeated occurrences within the
// window are silently dropped (the failure counter carries the volume).
func (b *boundedLogger) Warn(msg string, args ...any) {
	if b == nil || b.inner == nil {
		return
	}
	b.mu.Lock()
	now := time.Now()
	if last, ok := b.last[msg]; ok && now.Sub(last) < b.minimum {
		b.mu.Unlock()
		return
	}
	b.last[msg] = now
	b.mu.Unlock()
	b.inner.Warn(msg, args...)
}

// Handle satisfies ot.ErrorHandler for SDK/exporter errors.
func (b *boundedLogger) Handle(err error) {
	b.Warn("telemetry export failed", "error", err.Error())
}
