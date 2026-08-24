// SPDX-License-Identifier: Apache-2.0

// Package postgres implements Liftr's application persistence ports with pgx.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sithea-nou/liftr/internal/application"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("PostgreSQL pool is required")
	}
	return &Store{pool: pool}, nil
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()              { s.pool.Close() }
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) Within(ctx context.Context, fn func(application.UnitOfWork) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin PostgreSQL transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	repositories := &repositories{tx: tx}
	if err := fn(repositories); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return translateError(err)
	}
	return nil
}

type repositories struct {
	tx pgx.Tx
}

func (r *repositories) Resources() application.ResourceRepository                   { return r }
func (r *repositories) Operations() application.OperationRepository                 { return r }
func (r *repositories) Events() application.EventRepository                         { return r }
func (r *repositories) Executions() application.ExecutionRepository                 { return r }
func (r *repositories) Idempotency() application.IdempotencyRepository              { return r }
func (r *repositories) SubmissionAttempts() application.SubmissionAttemptRepository { return r }
func (r *repositories) Outbox() application.OutboxRepository                        { return r }
func (r *repositories) Outputs() application.ResourceOutputRepository               { return r }

func translateError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return application.ErrResourceNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return fmt.Errorf("%w: %s", application.ErrConcurrencyConflict, pgErr.Message)
		case "40001", "40P01":
			return fmt.Errorf("%w: %s", application.ErrRetryablePersistence, pgErr.Message)
		case "23503", "23514", "23000":
			return fmt.Errorf("%w: %s", application.ErrInvalidApplicationCall, pgErr.Message)
		}
	}
	return err
}
