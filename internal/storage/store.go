// Package storage owns the PostgreSQL connection pool and the
// transaction-scoped tenant context (TEN-REQ-007).
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/kavish924/multi-tenant-rag/internal/config"
)

// ErrNoTenant is returned when WithTenant is called without a tenant id.
var ErrNoTenant = errors.New("storage: tenant id is required")

// Store wraps the pool. Binaries connect as a member of the `app` role, so
// every tenant-owned table is filtered by forced RLS.
type Store struct {
	pool    *pgxpool.Pool
	cleanup func() error
}

// New wraps an existing pool (used by tests).
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, cleanup: func() error { return nil }}
}

// Open connects using either a DSN or the Cloud SQL connector.
func Open(ctx context.Context, c config.DB) (*Store, error) {
	pc, cleanup, err := PoolConfig(ctx, c)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		_ = cleanup()
		return nil, fmt.Errorf("storage: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		_ = cleanup()
		return nil, fmt.Errorf("storage: ping: %w", err)
	}
	return &Store{pool: pool, cleanup: cleanup}, nil
}

// PoolConfig builds a pgx pool config for either connection mode. The
// returned cleanup closes the Cloud SQL dialer, if one was created.
func PoolConfig(ctx context.Context, c config.DB) (*pgxpool.Config, func() error, error) {
	if c.URL != "" {
		pc, err := pgxpool.ParseConfig(c.URL)
		if err != nil {
			return nil, nil, fmt.Errorf("storage: parse DATABASE_URL: %w", err)
		}
		return pc, func() error { return nil }, nil
	}

	d, err := cloudsqlconn.NewDialer(ctx,
		cloudsqlconn.WithIAMAuthN(),
		cloudsqlconn.WithDefaultDialOptions(cloudsqlconn.WithPrivateIP()),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("storage: cloud sql dialer: %w", err)
	}
	// The connector handles TLS and the IAM token; the DSN carries only the
	// user and database name.
	dsn := fmt.Sprintf("user=%s dbname=%s sslmode=disable", dsnQuote(c.User), dsnQuote(c.Name))
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		_ = d.Close()
		return nil, nil, fmt.Errorf("storage: parse cloud sql config: %w", err)
	}
	instance := c.CloudSQLInstance
	pc.ConnConfig.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return d.Dial(ctx, instance)
	}
	return pc, d.Close, nil
}

// OpenSQL returns a database/sql handle for tools that need one (migrations).
func OpenSQL(ctx context.Context, c config.DB) (*sql.DB, func() error, error) {
	pc, cleanup, err := PoolConfig(ctx, c)
	if err != nil {
		return nil, nil, err
	}
	return stdlib.OpenDB(*pc.ConnConfig), cleanup, nil
}

// Close releases the pool and the dialer.
func (s *Store) Close() {
	s.pool.Close()
	_ = s.cleanup()
}

// Ping checks connectivity (readiness probe).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Pool exposes the pool for queries that are not tenant-scoped: the global
// users table and the closed set of system functions (TEN-REQ-010). A query
// on a tenant-owned table issued here returns zero rows under RLS.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// WithTenant runs fn in a transaction whose app.tenant_id is set with
// is_local = true, so the setting ends with the transaction and a pooled
// connection never carries a tenant into the next request (TEN-AC-007).
// The transaction commits only if fn returns nil.
func (s *Store) WithTenant(ctx context.Context, tenantID uuid.UUID, fn func(pgx.Tx) error) error {
	if tenantID == uuid.Nil {
		return ErrNoTenant
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
			return fmt.Errorf("storage: set tenant: %w", err)
		}
		return fn(tx)
	})
}

// dsnQuote quotes a value for a keyword/value connection string.
func dsnQuote(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(v) + "'"
}
