// Command outbox-relay publishes committed outbox rows (spec 006). Exactly
// one instance is active at a time (a Postgres advisory lock); extra
// instances wait on standby. Until a broker is configured it publishes to
// the log (OBX-AC-004).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kavish924/multi-tenant-rag/internal/config"
	"github.com/kavish924/multi-tenant-rag/internal/messaging"
	"github.com/kavish924/multi-tenant-rag/internal/observability"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
)

func main() {
	log, flush, err := telemetry("outbox-relay")
	if err != nil {
		log.Error("telemetry setup", "err", err)
		os.Exit(1)
	}
	err = run(log)
	flush()
	if err != nil {
		log.Error("outbox-relay exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.LoadRelay()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := storage.Open(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer store.Close()

	relay := &messaging.Relay{
		Pool:         store.Pool(),
		Publisher:    messaging.LogPublisher{Log: log},
		BatchSize:    cfg.BatchSize,
		PollInterval: cfg.PollInterval,
		MaxAttempts:  cfg.MaxAttempts,
		Log:          log,
	}
	log.Info("outbox-relay starting", "publisher", cfg.Publisher, "batch_size", cfg.BatchSize, "poll_interval", cfg.PollInterval.String())
	return relay.Run(ctx)
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
