// Command api serves the HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kavish924/multi-tenant-rag/internal/api"
	"github.com/kavish924/multi-tenant-rag/internal/auth"
	"github.com/kavish924/multi-tenant-rag/internal/config"
	"github.com/kavish924/multi-tenant-rag/internal/conversations"
	"github.com/kavish924/multi-tenant-rag/internal/documents"
	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
	"github.com/kavish924/multi-tenant-rag/internal/storage/objectstore"
	"github.com/kavish924/multi-tenant-rag/internal/tenants"
	"github.com/kavish924/multi-tenant-rag/internal/usage"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("api exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.LoadAPI()
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

	tokens := auth.NewTokens(cfg.JWTKey, cfg.JWTIssuer, cfg.JWTTTL)
	users, err := auth.NewService(store.Pool(), tokens)
	if err != nil {
		return err
	}

	objects, err := objectstore.NewFS(cfg.Objects.Dir)
	if err != nil {
		return err
	}

	// Questions must be embedded by the same provider as ingestion
	// (cmd/worker): llm.Fake until the Vertex AI provider lands.
	var embedder llm.EmbeddingProvider = llm.Fake{}
	// Gemini on Vertex AI lands with the GCP work; until then a
	// deterministic extractive fake answers from source 1.
	var generator llm.Generator = llm.FakeGenerator{}
	retriever := retrieval.NewHybrid(store, embedder, retrieval.Options{}, log)
	ask := &conversations.Handler{
		Retriever:  retriever,
		Generator:  generator,
		Usage:      usage.NewRecorder(store),
		EmbedModel: embedder.Model(),
		Log:        log,
	}

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: api.New(api.Deps{
			Users:     users,
			Tokens:    tokens,
			Tenants:   tenants.NewService(store),
			Documents: documents.NewService(store, objects, cfg.MaxUploadBytes),
			Retriever: retriever,
			Ask:       http.HandlerFunc(ask.Ask),
			Ready:     store.Ping,
			Log:       log,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Minute, // 50 MB uploads on slow links
		IdleTimeout:       120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", cfg.Addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Cloud Run sends SIGTERM and allows 10s before SIGKILL.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
