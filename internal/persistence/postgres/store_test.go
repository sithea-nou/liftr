// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sithea-nou/liftr/internal/application"
)

func TestTranslateErrorDistinguishesRetryablePersistenceFromConcurrency(t *testing.T) {
	tests := []struct {
		code string
		want error
	}{
		{code: "23505", want: application.ErrConcurrencyConflict},
		{code: "40001", want: application.ErrRetryablePersistence},
		{code: "40P01", want: application.ErrRetryablePersistence},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			translated := translateError(&pgconn.PgError{Code: test.code, Message: "test"})
			if !errors.Is(translated, test.want) {
				t.Fatalf("translateError() = %v, want %v", translated, test.want)
			}
			if test.want == application.ErrRetryablePersistence && errors.Is(translated, application.ErrConcurrencyConflict) {
				t.Fatalf("retryable persistence error was translated as concurrency conflict: %v", translated)
			}
		})
	}
}
