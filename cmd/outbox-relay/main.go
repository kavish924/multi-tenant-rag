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

	"github.com/kavish924/multi-tenant-rag/internal/config"
	"github.com/kavish924/multi-tenant-rag/internal/messaging"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("outbox-relay exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.LoadRelay()
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err == nil {
		log = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
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
