// Command worker runs background jobs from the Postgres queue (spec 004).
// Registered kinds: ingest (005). cleanup (013) lands later; claim_job only
// returns kinds that have a handler, so cleanup jobs wait in the queue.
//
// The same binary is the parser helper: run with extract.HelperArg it
// extracts one file from stdin under resource limits and exits (ING-REQ-002).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/kavish924/multi-tenant-rag/internal/config"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/embed"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/extract"
	"github.com/kavish924/multi-tenant-rag/internal/jobs"
	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
	"github.com/kavish924/multi-tenant-rag/internal/storage/objectstore"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == extract.HelperArg {
		extract.HelperMain() // never returns
	}
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

	objects, err := objectstore.NewFS(cfg.Objects.Dir)
	if err != nil {
		return err
	}
	helper, err := extract.SelfCommand()
	if err != nil {
		return err
	}
	var provider llm.EmbeddingProvider = llm.Fake{} // EMBEDDER=fake; Vertex AI lands with the GCP work
	log.Info("embedding provider", "model", provider.Model(), "dim", provider.Dim())

	pipeline := &ingestion.Pipeline{
		Objects:   objects,
		Extractor: &extract.Isolated{Command: helper},
		Embedder:  &embed.Batcher{Provider: provider},
		Log:       log,
	}

	queue := jobs.NewQueue(store, jobs.DefaultLease)
	hostname, _ := os.Hostname()
	r := &jobs.Runner{
		Queue:       queue,
		Handlers:    map[string]jobs.Handler{ingestion.JobKind: pipeline.Handle},
		Tenant:      jobs.StoreScoper(store),
		WorkerID:    hostname + "-" + strconv.Itoa(os.Getpid()),
		Concurrency: cfg.Concurrency,
		Lease:       queue.Lease(),
		Log:         log,
	}
	return r.Run(ctx)
}
