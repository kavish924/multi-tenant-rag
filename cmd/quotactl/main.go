// Command quotactl is the operator path for per-tenant quota overrides
// (spec 011, QTA-REQ-008). It writes tenant_quotas, which the app role can
// only read, so run it with the migrator's credentials (Cloud Shell via the
// Cloud SQL connector, or DATABASE_URL in dev). Changes apply to running
// services within the limiter cache TTL (5 s), with no redeploy.
//
//	quotactl show  -tenant <id>
//	quotactl set   -tenant <id> [-uploads-day N] [-questions-day N] [-embed-tokens-month N]
//	               [-generate-tokens-month N] [-concurrent-asks N] [-concurrent-jobs N]
//	quotactl reset -tenant <id>   # back to platform defaults
//
// set replaces the whole override row: a flag left out returns that
// dimension to the platform default.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/config"
	"github.com/kavish924/multi-tenant-rag/internal/quotas"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "quotactl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: quotactl show|set|reset -tenant <id> [flags]")
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	tenant := fs.String("tenant", "", "tenant id")
	fields := map[string]*int64{}
	for _, name := range []string{"uploads-day", "questions-day", "embed-tokens-month", "generate-tokens-month", "concurrent-asks", "concurrent-jobs"} {
		fields[name] = fs.Int64(name, -1, "override (-1 = keep the platform default)")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	tid, err := uuid.Parse(*tenant)
	if err != nil {
		return errors.New("-tenant must be a tenant id")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dbc, err := config.LoadDB()
	if err != nil {
		return err
	}
	store, err := storage.Open(ctx, dbc)
	if err != nil {
		return err
	}
	defer store.Close()

	switch cmd {
	case "show":
	case "reset":
		if err := quotas.SetOverrides(ctx, store, tid, quotas.Overrides{}); err != nil {
			return err
		}
	case "set":
		opt := func(name string) *int64 {
			if v := *fields[name]; v >= 0 {
				return &v
			}
			return nil
		}
		o := quotas.Overrides{
			UploadsDay: opt("uploads-day"), QuestionsDay: opt("questions-day"),
			EmbedTokensMonth: opt("embed-tokens-month"), GenerateTokensMonth: opt("generate-tokens-month"),
			ConcurrentAsks: opt("concurrent-asks"), ConcurrentJobs: opt("concurrent-jobs"),
		}
		if err := quotas.SetOverrides(ctx, store, tid, o); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}

	var o quotas.Overrides
	err = store.WithTenant(ctx, tid, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT uploads_day, questions_day, embed_tokens_month, generate_tokens_month, concurrent_asks, concurrent_jobs
			FROM tenant_quotas WHERE tenant_id = $1`, tid).
			Scan(&o.UploadsDay, &o.QuestionsDay, &o.EmbedTokensMonth, &o.GenerateTokensMonth, &o.ConcurrentAsks, &o.ConcurrentJobs)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(map[string]any{"tenant_id": tid, "overrides": o}, "", "  ")
	fmt.Println(string(out))
	return nil
}
