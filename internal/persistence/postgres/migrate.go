// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const migrationLockID int64 = 0x4c494654520006

type migration struct {
	version  int64
	name     string
	checksum string
	sql      string
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS liftr_schema_migrations (
		version bigint PRIMARY KEY,
		name text NOT NULL,
		checksum text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
	)`); err != nil {
		return fmt.Errorf("create migration history: %w", err)
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return err
	}
	if err := validateMigrationPrefix(migrations, applied); err != nil {
		return err
	}
	for _, item := range migrations[len(applied):] {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", item.version, err)
		}
		if _, err := tx.Exec(ctx, item.sql); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %d: %w", item.version, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO liftr_schema_migrations(version, name, checksum) VALUES ($1, $2, $3)", item.version, item.name, item.checksum); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %d: %w", item.version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %d: %w", item.version, err)
		}
	}
	return nil
}

func VerifySchema(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire schema verification connection: %w", err)
	}
	defer conn.Release()
	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return err
	}
	if err := validateMigrationPrefix(migrations, applied); err != nil {
		return err
	}
	if len(applied) != len(migrations) {
		return fmt.Errorf("schema has %d applied migrations, want %d", len(applied), len(migrations))
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(names)
	result := make([]migration, 0, len(names))
	var previous int64
	for _, name := range names {
		base := path.Base(name)
		parts := strings.SplitN(base, "_", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid migration filename %q", base)
		}
		version, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || version <= previous {
			return nil, fmt.Errorf("invalid migration version in %q", base)
		}
		contents, err := migrationFiles.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", base, err)
		}
		digest := sha256.Sum256(contents)
		result = append(result, migration{version: version, name: base, checksum: hex.EncodeToString(digest[:]), sql: string(contents)})
		previous = version
	}
	return result, nil
}

func appliedMigrations(ctx context.Context, conn *pgxpool.Conn) ([]migration, error) {
	rows, err := conn.Query(ctx, "SELECT version, name, checksum FROM liftr_schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read migration history: %w", err)
	}
	defer rows.Close()
	var result []migration
	for rows.Next() {
		var item migration
		if err := rows.Scan(&item.version, &item.name, &item.checksum); err != nil {
			return nil, fmt.Errorf("scan migration history: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migration history: %w", err)
	}
	return result, nil
}

func validateMigrationPrefix(available, applied []migration) error {
	if len(applied) > len(available) {
		return fmt.Errorf("database has %d migrations but this binary has %d", len(applied), len(available))
	}
	for i, existing := range applied {
		candidate := available[i]
		if existing.version != candidate.version {
			return fmt.Errorf("applied migration %d is not the expected version %d at position %d", existing.version, candidate.version, i+1)
		}
		if existing.name != candidate.name || existing.checksum != candidate.checksum {
			return fmt.Errorf("migration %d checksum or name mismatch", existing.version)
		}
	}
	return nil
}
