//go:build integration

// Package pgtest starts a disposable PostgreSQL for integration tests,
// applies the migrations as the owning role, and connects the application
// pool as a login role that is only a member of `app` — the production
// shape, so RLS is exercised for real.
//
// If TEST_DATABASE_URL is set (a superuser DSN, e.g. a Cloud Build service
// container), a fresh database is created on that server; otherwise a
// pgvector/pgvector container is started with testcontainers.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kavish924/multi-tenant-rag/internal/storage"
)

// Image matches the Cloud SQL major version (docs/tech-stack.md).
const Image = "pgvector/pgvector:pg17"

// AppLogin is the login role the application pool uses in tests.
const AppLogin = "app_login"

const appLoginPassword = "app_login_test_password"

// Env is one migrated database.
type Env struct {
	// Admin connects as the owning superuser; tests use it to seed and to
	// inspect catalogs, never to assert isolation.
	Admin *pgxpool.Pool
	// App connects as AppLogin (member of `app` only).
	App   *pgxpool.Pool
	Store *storage.Store

	appConfig *pgxpool.Config
	close     []func()
}

// Start creates the environment. Call from TestMain; Close when done.
func Start(ctx context.Context) (*Env, error) {
	env := &Env{}
	adminDSN, err := serverDSN(ctx, env)
	if err != nil {
		env.Close()
		return nil, err
	}

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		env.Close()
		return nil, err
	}
	env.close = append(env.close, admin.Close)
	env.Admin = admin

	db := stdlib.OpenDBFromPool(admin)
	err = storage.Migrate(ctx, db)
	_ = db.Close()
	if err != nil {
		env.Close()
		return nil, err
	}

	if _, err := admin.Exec(ctx, fmt.Sprintf(`
		DO $$ BEGIN
		  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%s') THEN
		    CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS;
		  END IF;
		END $$`, AppLogin, AppLogin, appLoginPassword)); err != nil {
		env.Close()
		return nil, err
	}
	if _, err := admin.Exec(ctx, "GRANT app TO "+AppLogin); err != nil {
		env.Close()
		return nil, err
	}

	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		env.Close()
		return nil, err
	}
	cfg.ConnConfig.User = AppLogin
	cfg.ConnConfig.Password = appLoginPassword
	env.appConfig = cfg

	app, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
	if err != nil {
		env.Close()
		return nil, err
	}
	env.close = append(env.close, app.Close)
	env.App = app
	env.Store = storage.New(app)
	return env, nil
}

// NewAppPool opens an extra pool as AppLogin with at most maxConns
// connections (used by the pool-reuse isolation test).
func (e *Env) NewAppPool(ctx context.Context, maxConns int32) (*pgxpool.Pool, error) {
	cfg := e.appConfig.Copy()
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Close tears everything down in reverse order.
func (e *Env) Close() {
	for i := len(e.close) - 1; i >= 0; i-- {
		e.close[i]()
	}
}

// Truncate empties every application table (all of public except the
// migration bookkeeping table) between tests.
func (e *Env) Truncate(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	rows, err := e.Admin.Query(ctx, `
		SELECT quote_ident(tablename) FROM pg_tables
		WHERE schemaname = 'public' AND tablename <> 'goose_db_version'`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := e.Admin.Exec(ctx, "TRUNCATE "+strings.Join(tables, ", ")+" CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// SeedTenant inserts a tenant plus memberships directly as the owner role.
func (e *Env) SeedTenant(t testing.TB, name string, members map[uuid.UUID]string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := e.Admin.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ($1, $2)`, id, name); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	for u, role := range members {
		if _, err := e.Admin.Exec(ctx,
			`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, $3)`, id, u, role); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	return id
}

// SeedUser inserts a user with an unusable password hash.
func (e *Env) SeedUser(t testing.TB, email string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.Admin.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func serverDSN(ctx context.Context, env *Env) (string, error) {
	if base := os.Getenv("TEST_DATABASE_URL"); base != "" {
		return freshDatabase(ctx, env, base)
	}
	ctr, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase("app"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		return "", fmt.Errorf("pgtest: start container: %w", err)
	}
	env.close = append(env.close, func() { _ = testcontainers.TerminateContainer(ctr) })
	return ctr.ConnectionString(ctx, "sslmode=disable")
}

// freshDatabase creates a uniquely named database on an existing server so
// packages running in parallel do not share state.
func freshDatabase(ctx context.Context, env *Env, base string) (string, error) {
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		return "", fmt.Errorf("pgtest: connect TEST_DATABASE_URL: %w", err)
	}
	defer conn.Close(ctx)
	name := "t_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return "", err
	}
	env.close = append(env.close, func() {
		c, err := pgx.Connect(context.Background(), base)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" {
		return "", fmt.Errorf("pgtest: TEST_DATABASE_URL must be a postgres:// URL")
	}
	u.Path = "/" + name
	return u.String(), nil
}
