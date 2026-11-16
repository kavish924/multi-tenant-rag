// Command migrate applies database migrations. On Cloud Run it runs as a job
// under the sa-migrate service account, connecting as the `migrator` IAM
// database user.
//
// Environment:
//
//	DATABASE_URL or CLOUDSQL_INSTANCE + DB_USER + DB_NAME   (see internal/config)
//	APP_DB_LOGIN_ROLES   comma-separated login roles to add to the `app`
//	                     group role, e.g. the IAM database users of
//	                     sa-api, sa-worker and sa-relay
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/kavish924/multi-tenant-rag/internal/config"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(); err != nil {
		log.Error("migrate failed", "err", err)
		os.Exit(1)
	}
	log.Info("migrations applied")
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cfg, err := config.LoadDB()
	if err != nil {
		return err
	}
	db, cleanup, err := storage.OpenSQL(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = cleanup() }()
	defer db.Close()

	if err := storage.Migrate(ctx, db); err != nil {
		return err
	}
	return storage.GrantAppRole(ctx, db, config.List("APP_DB_LOGIN_ROLES"))
}
