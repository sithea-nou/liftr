// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"context"
	"errors"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
)

// instrumentedTransactions decorates the application TransactionRunner port
// with bounded transaction metrics. The application layer is unaware of it;
// composition wraps the durable store once and hands the decorated runner to
// both the service and the worker.
type instrumentedTransactions struct {
	inner application.TransactionRunner
	tel   *Telemetry
}

// InstrumentTransactions wraps a TransactionRunner with persistence metrics.
func InstrumentTransactions(inner application.TransactionRunner, tel *Telemetry) (application.TransactionRunner, error) {
	if inner == nil || tel == nil || tel.instruments == nil {
		return nil, errors.New("instrumented transactions dependencies are required")
	}
	return &instrumentedTransactions{inner: inner, tel: tel}, nil
}

// Bounded transaction result classifications.
const (
	TxResultCommitted = "committed"
	TxResultRetryable = "retryable"
	TxResultConflict  = "conflict"
	TxResultInvalid   = "invalid"
	TxResultError     = "error"
)

func (d *instrumentedTransactions) Within(ctx context.Context, fn func(application.UnitOfWork) error) error {
	started := time.Now()
	err := d.inner.Within(ctx, fn)
	d.tel.TransactionFinished(classifyTxError(err), time.Since(started))
	return err
}

func classifyTxError(err error) string {
	switch {
	case err == nil:
		return TxResultCommitted
	case errors.Is(err, application.ErrRetryablePersistence):
		return TxResultRetryable
	case errors.Is(err, application.ErrConcurrencyConflict):
		return TxResultConflict
	case errors.Is(err, application.ErrInvalidApplicationCall):
		return TxResultInvalid
	default:
		return TxResultError
	}
}
