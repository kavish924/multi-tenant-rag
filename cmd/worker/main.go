// Command worker runs background jobs from the Postgres queue (spec 004).
// Registered kinds: ingest (005), cleanup and purge (013). It also runs the
// retention sweeper (013).
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
	"time"

	"github.com/kavish924/multi-tenant-rag/internal/breaker"
	"github.com/kavish924/multi-tenant-rag/internal/config"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/cleanup"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/embed"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/extract"
	"github.com/kavish924/multi-tenant-rag/internal/jobs"
	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/observability"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
	"github.com/kavish924/multi-tenant-rag/internal/storage/objectstore"
	"github.com/kavish924/multi-tenant-rag/internal/usage"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == extract.HelperArg {
		extract.HelperMain() // never returns
	}
	log, flush, err := telemetry("worker")
	if err != nil {
		log.Error("telemetry setup", "err", err)
		os.Exit(1)
	}
	err = run(log)
	flush()
	if err != nil {
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
	// EMBEDDER=fake; Vertex AI lands with the GCP work. An open circuit is a
	// retryable error, so the job retry ladder is unchanged (QTA-REQ-005).
	var provider llm.EmbeddingProvider = breaker.Embedder{
		EmbeddingProvider: llm.Fake{},
		B:                 breaker.New("embed", cfg.Quotas.BreakerThreshold, cfg.Quotas.BreakerOpenFor),
	}
	log.Info("embedding provider", "model", provider.Model(), "dim", provider.Dim())

	pipeline := &ingestion.Pipeline{
		Objects:   objects,
		Extractor: &extract.Isolated{Command: helper},
		Embedder:  &embed.Batcher{Provider: provider},
		Usage:     usage.NewRecorder(store),
		Log:       log,
	}

	// Deletion (spec 013): cleanup and purge jobs, and the retention sweeper
	// (one active across workers, via an advisory lock).
	cleaner := &cleanup.Cleaner{Objects: objects, Log: log}
	purger := &cleanup.Purger{Cleaner: cleaner}
	sweeper := &cleanup.Sweeper{
		Pool: store.Pool(), Store: store, Objects: objects,
		Retention: cfg.Retention, Interval: cfg.SweepInterval, OrphanGrace: cfg.OrphanGrace, Log: log,
	}
	go sweeper.Run(ctx)

	queue := jobs.NewQueue(store, jobs.DefaultLease)
	queue.SetTenantCap(int(cfg.Quotas.ConcurrentJobs)) // QTA-REQ-002 worker gate
	// jobs_queue_depth / jobs_queue_age_seconds (OBS-REQ-004).
	if err := observability.M().RegisterQueueGauges(queue.Stats); err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	r := &jobs.Runner{
		Queue: queue,
		Handlers: map[string]jobs.Handler{
			ingestion.JobKind:  pipeline.Handle,
			cleanup.JobCleanup: cleaner.Handle,
			cleanup.JobPurge:   purger.Handle,
		},
		Tenant:      jobs.StoreScoper(store),
		WorkerID:    hostname + "-" + strconv.Itoa(os.Getpid()),
		Concurrency: cfg.Concurrency,
		Lease:       queue.Lease(),
		Log:         log,
	}
	return r.Run(ctx)
}

// telemetry installs the Cloud Logging JSON logger and OpenTelemetry
// (spec 010); the returned func flushes telemetry on exit.
func telemetry(service string) (*slog.Logger, func(), error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		level = slog.LevelInfo
	}
	log := observability.NewLogger(os.Stdout, level)
	slog.SetDefault(log)
	oc, err := observability.ConfigFromEnv(service)
	if err != nil {
		return log, func() {}, err
	}
	shutdown, err := observability.Setup(context.Background(), oc)
	if err != nil {
		return log, func() {}, err
	}
	return log, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}, nil
}
