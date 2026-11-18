package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kavish924/multi-tenant-rag/internal/auth"
	"github.com/kavish924/multi-tenant-rag/internal/config"
	"github.com/kavish924/multi-tenant-rag/internal/conversations"
	"github.com/kavish924/multi-tenant-rag/internal/documents"
	"github.com/kavish924/multi-tenant-rag/internal/evaluation"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/chunk"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/cleanup"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/embed"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/extract"
	"github.com/kavish924/multi-tenant-rag/internal/jobs"
	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/reranking"
	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
	"github.com/kavish924/multi-tenant-rag/internal/storage/objectstore"
	"github.com/kavish924/multi-tenant-rag/internal/tenants"
)

// runConfig is one evaluated configuration (the knobs A/B compares).
type runConfig struct {
	name      string
	rerank    bool
	topK      int
	chunkMax  int
	generator string
}

func (c *runConfig) register(fs *flag.FlagSet, prefix, def string) {
	fs.StringVar(&c.name, prefix+"name", def, "configuration name")
	fs.BoolVar(&c.rerank, prefix+"rerank", false, "enable the rerank stage (spec 009)")
	fs.IntVar(&c.topK, prefix+"top-k", conversations.DefaultTopK, "hits in the prompt")
	fs.IntVar(&c.chunkMax, prefix+"chunk-max", chunk.Defaults.MaxTokens, "chunk hard ceiling in tokens")
	fs.StringVar(&c.generator, prefix+"generator", "fake", "generator: fake (Vertex AI lands with the GCP work)")
}

func (c runConfig) realGenerator() bool { return c.generator != "fake" }

func (c runConfig) chunking() chunk.Options {
	o := chunk.Defaults
	if c.chunkMax > 0 && c.chunkMax != o.MaxTokens {
		o.MaxTokens = c.chunkMax
		o.TargetTokens = min(o.TargetTokens, c.chunkMax*5/8)
		o.MinTokens = min(o.MinTokens, o.TargetTokens*3/5)
	}
	return o
}

// env is one prepared run: its tenants, ingested corpus and the system
// under test. It implements evaluation.System.
type env struct {
	log       *slog.Logger
	admin     *pgxpool.Pool
	store     *storage.Store
	objDir    string
	objects   *objectstore.FS
	tenants   map[string]uuid.UUID // corpus key → tenant id
	labels    map[uuid.UUID]string // chunk id → label
	embedder  llm.EmbeddingProvider
	generator llm.Generator
	retriever *retrieval.Hybrid
	handler   *conversations.Handler
	user      uuid.UUID
}

const evalRole = "eval_app"

func setup(ctx context.Context, log *slog.Logger, dir string, cfg runConfig) (*env, error) {
	if cfg.realGenerator() {
		return nil, fmt.Errorf("generator %q not available yet: only fake until the Vertex AI provider lands", cfg.generator)
	}
	adminDSN := os.Getenv("DATABASE_URL")
	if adminDSN == "" {
		return nil, errors.New("DATABASE_URL (an admin DSN that may migrate) is required")
	}
	// Migrations, then a login role that is a member of app only.
	sqlDB, closeSQL, err := storage.OpenSQL(ctx, config.DB{URL: adminDSN})
	if err != nil {
		return nil, err
	}
	defer func() { _ = closeSQL() }()
	if err := storage.Migrate(ctx, sqlDB); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return nil, err
	}
	pw := randomHex(16)
	var stmt string
	if err := admin.QueryRow(ctx, `SELECT CASE WHEN EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)
		THEN format('ALTER ROLE %I LOGIN PASSWORD %L', $1::text, $2::text)
		ELSE format('CREATE ROLE %I LOGIN PASSWORD %L', $1::text, $2::text) END`, evalRole, pw).Scan(&stmt); err != nil {
		admin.Close()
		return nil, err
	}
	if _, err := admin.Exec(ctx, stmt); err != nil {
		admin.Close()
		return nil, fmt.Errorf("eval role: %w", err)
	}
	if err := storage.GrantAppRole(ctx, sqlDB, []string{evalRole}); err != nil {
		admin.Close()
		return nil, err
	}
	u, err := url.Parse(adminDSN)
	if err != nil {
		admin.Close()
		return nil, err
	}
	u.User = url.UserPassword(evalRole, pw)
	store, err := storage.Open(ctx, config.DB{URL: u.String()})
	if err != nil {
		admin.Close()
		return nil, err
	}

	objDir, err := os.MkdirTemp("", "eval-objects-*")
	if err != nil {
		return nil, err
	}
	objects, err := objectstore.NewFS(objDir)
	if err != nil {
		return nil, err
	}
	e := &env{log: log, admin: admin, store: store, objDir: objDir, objects: objects,
		tenants: map[string]uuid.UUID{}, labels: map[uuid.UUID]string{},
		embedder: llm.Fake{}, generator: llm.FakeGenerator{}, user: uuid.New()}

	// Fresh tenants per run, so runs never see each other's data.
	run := randomHex(4)
	keys, err := corpusTenants(dir)
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		id := uuid.New()
		if _, err := admin.Exec(ctx, `INSERT INTO tenants (id, name) VALUES ($1, $2)`, id, "eval-"+key+"-"+run); err != nil {
			return nil, err
		}
		e.tenants[key] = id
	}
	if err := e.ingest(ctx, dir, cfg); err != nil {
		return nil, fmt.Errorf("ingest: %w", err)
	}
	if err := e.loadLabels(ctx); err != nil {
		return nil, err
	}

	e.retriever = retrieval.NewHybrid(store, e.embedder, retrieval.Options{}, log)
	var stage *reranking.Stage
	if cfg.rerank {
		stage = &reranking.Stage{Enabled: true, Reranker: &reranking.LLM{Generator: e.generator}, Keep: cfg.topK, Log: log}
	}
	e.handler = &conversations.Handler{
		Retriever: e.retriever, Generator: e.generator, Rerank: stage,
		EmbedModel: e.embedder.Model(), TopK: cfg.topK, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	log.Info("eval environment ready", "config", cfg.name, "tenants", len(e.tenants), "chunks", len(e.labels))
	return e, nil
}

func corpusTenants(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "corpus"))
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, en := range entries {
		if en.IsDir() {
			keys = append(keys, en.Name())
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// ingest uploads every corpus file and runs the ingest jobs through the
// real pipeline (isolated extractor helper, chunker, embedder, index).
func (e *env) ingest(ctx context.Context, dir string, cfg runConfig) error {
	docs := documents.NewService(e.store, e.objects, 0)
	for key, tid := range e.tenants {
		files, err := filepath.Glob(filepath.Join(dir, "corpus", key, "*.md"))
		if err != nil {
			return err
		}
		for _, f := range files {
			body, err := os.ReadFile(f) //nolint:gosec // fixture corpus
			if err != nil {
				return err
			}
			if _, err := docs.Upload(ctx, documents.UploadInput{
				TenantID: tid, Filename: filepath.Base(f), DeclaredType: documents.TypeText, Body: bytes.NewReader(body),
			}); err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
		}
	}
	helper, err := extract.SelfCommand()
	if err != nil {
		return err
	}
	p := &ingestion.Pipeline{
		Objects: e.objects, Extractor: &extract.Isolated{Command: helper},
		Embedder: &embed.Batcher{Provider: e.embedder}, Chunking: cfg.chunking(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	q := jobs.NewQueue(e.store, 0)
	q.SetTenantCap(1 << 20)
	scope := jobs.StoreScoper(e.store)
	for {
		j, err := q.Claim(ctx, "eval", []string{ingestion.JobKind})
		if err != nil {
			return err
		}
		if j == nil {
			return nil
		}
		if err := p.Handle(ctx, *j, scope(j.TenantID)); err != nil {
			return fmt.Errorf("ingest job %s: %w", j.ID, err)
		}
		if err := q.Complete(ctx, *j, "eval"); err != nil {
			return err
		}
	}
}

// loadLabels maps every chunk of the run's tenants to its stable label.
// It reads as admin, so a foreign chunk would still get its own tenant's
// label and be counted as a leak.
func (e *env) loadLabels(ctx context.Context) error {
	keyOf := map[uuid.UUID]string{}
	ids := make([]uuid.UUID, 0, len(e.tenants))
	for k, id := range e.tenants {
		keyOf[id] = k
		ids = append(ids, id)
	}
	rows, err := e.admin.Query(ctx, `
		SELECT c.id, d.tenant_id, d.filename, c.ordinal
		FROM chunks c JOIN documents d ON d.id = c.document_id
		WHERE d.tenant_id = ANY($1)`, ids)
	if err != nil {
		return err
	}
	var chunkID, tenantID uuid.UUID
	var file string
	var ordinal int
	_, err = pgx.ForEachRow(rows, []any{&chunkID, &tenantID, &file, &ordinal}, func() error {
		e.labels[chunkID] = evaluation.Label(keyOf[tenantID], file, ordinal)
		return nil
	})
	return err
}

// Search implements evaluation.System.
func (e *env) Search(ctx context.Context, c evaluation.Case, k int) ([]evaluation.Hit, error) {
	tid, ok := e.tenants[c.Tenant]
	if !ok {
		return nil, fmt.Errorf("unknown tenant %q", c.Tenant)
	}
	hits, err := e.retriever.Search(ctx, tid, c.Question, k)
	if err != nil {
		return nil, err
	}
	out := make([]evaluation.Hit, len(hits))
	for i, h := range hits {
		out[i] = evaluation.Hit{Label: e.labels[h.ChunkID], Content: h.Content}
	}
	return out, nil
}

// Ask implements evaluation.System through the real /ask handler.
func (e *env) Ask(ctx context.Context, c evaluation.Case) (evaluation.Answer, error) {
	tid, ok := e.tenants[c.Tenant]
	if !ok {
		return evaluation.Answer{}, fmt.Errorf("unknown tenant %q", c.Tenant)
	}
	body, _ := json.Marshal(map[string]string{"question": c.Question})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/ask", bytes.NewReader(body))
	rctx := tenants.WithMembership(auth.WithUser(req.Context(), e.user), tenants.Membership{TenantID: tid, Role: tenants.RoleMember})
	rec := httptest.NewRecorder()
	e.handler.Ask(rec, req.WithContext(rctx))
	if rec.Code != http.StatusOK {
		return evaluation.Answer{}, fmt.Errorf("ask: status %d: %s", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	return e.parseSSE(rec.Body)
}

func (e *env) parseSSE(r io.Reader) (evaluation.Answer, error) {
	var a evaluation.Answer
	var text strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := []byte(strings.TrimPrefix(line, "data: "))
			switch event {
			case "token":
				var t struct {
					T string `json:"t"`
				}
				_ = json.Unmarshal(data, &t)
				text.WriteString(t.T)
			case "citation":
				var c struct {
					N       int       `json:"n"`
					ChunkID uuid.UUID `json:"chunk_id"`
				}
				_ = json.Unmarshal(data, &c)
				a.Citations = append(a.Citations, evaluation.Citation{N: c.N, Label: e.labels[c.ChunkID]})
			case "done":
				var d struct {
					Insufficient bool `json:"insufficient_context"`
				}
				_ = json.Unmarshal(data, &d)
				a.Insufficient = d.Insufficient
			case "refusal":
				a.Refused = true
			case "error":
				return a, fmt.Errorf("generation error: %s", data)
			}
		}
	}
	a.Text = text.String()
	return a, sc.Err()
}

// close purges the run's tenants with the spec 013 purge job (dogfooding),
// then removes the tenant rows and the object directory.
func (e *env) close(ctx context.Context, keep bool) {
	defer func() {
		e.store.Close()
		e.admin.Close()
		_ = os.RemoveAll(e.objDir)
	}()
	if keep {
		e.log.Info("keeping eval tenants", "tenants", e.tenants)
		return
	}
	purger := &cleanup.Purger{Cleaner: &cleanup.Cleaner{Objects: e.objects, Log: e.log}}
	scope := jobs.StoreScoper(e.store)
	for key, tid := range e.tenants {
		if err := purger.Handle(ctx, jobs.Job{ID: uuid.New(), TenantID: tid, Kind: cleanup.JobPurge, Payload: []byte(`{}`)}, scope(tid)); err != nil {
			e.log.Error("purge eval tenant", "tenant", key, "err", err)
			continue
		}
		if _, err := e.admin.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tid); err != nil {
			e.log.Error("delete eval tenant", "tenant", key, "err", err)
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
