// Package db is the PostgreSQL side of the Go server.
//
// The server targets PostgreSQL from day one: there is no Microsoft SQL Server
// code path here and there never will be. The legacy engine is read-only
// reference; its row data is moved later, by a Python script, once this schema
// has stopped changing. See docker-port\history\postgres-migration.md.
//
// Migrations are versioned files in migrations\, embedded in the binary, and
// applied with goose. Embedding means the deployable binary carries its own
// schema - there is no "run the .sql files by hand" step to forget.
package db

import (
	"context"
	"embed"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// EnvDSN is the environment variable holding the PostgreSQL connection string.
const EnvDSN = "GE_PG_DSN"

// DefaultDSN is the development database from history\postgres-migration.md:
// the granado_pg container on host port 5433. It is a convenience for a local
// run only - deployments set GE_PG_DSN.
const DefaultDSN = "postgres://granado:Granado%400504@localhost:5433/ge_data?sslmode=disable"

// DSN returns the connection string to use: GE_PG_DSN when set, else the
// development default.
func DSN() string {
	if v := os.Getenv(EnvDSN); v != "" {
		return v
	}
	return DefaultDSN
}

// Open connects to PostgreSQL and proves the connection works. A pool that
// has not been pinged reports nothing until the first query, which is too late
// to find out the database is unreachable.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// Migrate applies every pending migration, in file order, inside a
// transaction each. It is safe to call on every start: goose records what has
// already run.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	sqldb := stdlib.OpenDBFromPool(pool)
	defer sqldb.Close()

	if err := goose.UpContext(ctx, sqldb, "migrations"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}
