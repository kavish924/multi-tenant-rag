package storage

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/pressly/goose/v3"

	"github.com/kavish924/multi-tenant-rag/migrations"
)

// Migrate applies every pending migration. It must run as the migrating role
// (`migrator` on Cloud SQL), never as a member of `app`.
func Migrate(ctx context.Context, db *sql.DB) error {
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return fmt.Errorf("storage: migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("storage: migrate up: %w", err)
	}
	return nil
}

// GrantAppRole makes each login role (the Cloud SQL IAM database users of the
// api, worker and relay service accounts) a member of the `app` group role.
// It is idempotent: re-granting an existing membership is a no-op.
func GrantAppRole(ctx context.Context, db *sql.DB, logins []string) error {
	for _, login := range logins {
		stmt := "GRANT app TO " + pgx.Identifier{login}.Sanitize()
		if _, err := db.ExecContext(ctx, stmt); err != nil { //nolint:gosec // identifier is quoted with pgx.Identifier.Sanitize; GRANT cannot take a bind parameter
			return fmt.Errorf("storage: %s: %w", stmt, err)
		}
	}
	return nil
}
