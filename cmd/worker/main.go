// Command worker runs background jobs from the Postgres queue (spec 004).
// Job kinds are registered as their specs land: ingest (005), cleanup (013).
// Until then the worker claims nothing, because claim_job only returns kinds
// that have a handler.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/kavish924/multi-tenant-rag/internal/config"
	"github.com/kavish924/multi-tenant-rag/internal/jobs"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("worker exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	// Cloud Run sends SIGTERM on scale-in and redeploy (JOB-AC-009).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := storage.Open(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer store.Close()

	queue := jobs.NewQueue(store, jobs.DefaultLease)
	hostname, _ := os.Hostname()
	r := &jobs.Runner{
		Queue:       queue,
		Handlers:    map[string]jobs.Handler{},
		Tenant:      jobs.StoreScoper(store),
		WorkerID:    hostname + "-" + strconv.Itoa(os.Getpid()),
		Concurrency: cfg.Concurrency,
		Lease:       queue.Lease(),
		Log:         log,
	}
	return r.Run(ctx)
}
