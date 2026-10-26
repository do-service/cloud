// Package db owns the Postgres connection pool and schema migrations.
// Queries are hand-written SQL over pgx — no ORM, no code generation.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// NewPool opens a pgx connection pool and pings it, so a wrong DATABASE_URL
// fails startup instead of the first request.
//
// statementTimeout > 0 sets Postgres' statement_timeout on every pooled
// connection: a query with a bad plan gets cancelled instead of pinning a
// pool slot while every other request queues behind it. 0 leaves it off.
func NewPool(ctx context.Context, databaseURL string, maxConns, minConns int32, statementTimeout time.Duration) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse pool config: %w", err)
	}
	poolConfig.MaxConns = maxConns
	poolConfig.MinConns = minConns
	if statementTimeout > 0 {
		poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(statementTimeout.Milliseconds(), 10)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// Migrate applies all pending goose migrations using a plain database/sql
// connection (goose doesn't speak pgx pool directly).
func Migrate(databaseURL string) error {
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open sql.DB: %w", err)
	}
	defer sqlDB.Close()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}
