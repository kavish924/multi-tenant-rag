// Package retrieval is tenant-scoped hybrid search (spec 007): a vector leg
// (pgvector HNSW, cosine) and a full-text leg (tsvector GIN,
// websearch_to_tsquery) fused with Reciprocal Rank Fusion, excluding deleted
// documents and capping chunks per document.
package retrieval

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
)

//go:embed hybrid.sql
var hybridSQL string

// Defaults (RET-REQ-002/004/006).
const (
	DefaultCandidatesPerLeg = 50
	DefaultK                = 60
	DefaultPerDocumentCap   = 5
	DefaultLimit            = 50
	// MaxLimit is the largest result set: both legs' candidates.
	MaxLimit = 2 * DefaultCandidatesPerLeg
	// MaxQuestionRunes bounds the question before any provider call; the
	// guardrails input layer (012) may tighten it.
	MaxQuestionRunes = 2000
)

// Errors the API maps to statuses.
var (
	ErrEmptyQuestion    = errors.New("question is empty")
	ErrQuestionTooLong  = fmt.Errorf("question exceeds %d characters", MaxQuestionRunes)
	ErrEmbeddingFailure = errors.New("question embedding failed")
)

// Hit is one ranked, citation-ready chunk (RET-REQ-001).
type Hit struct {
	ChunkID    uuid.UUID `json:"chunk_id"`
	DocumentID uuid.UUID `json:"document_id"`
	Filename   string    `json:"filename"`
	Content    string    `json:"content"`
	Page       int       `json:"page,omitempty"`
	Heading    string    `json:"heading,omitempty"`
	Score      float64   `json:"score"`
	Rank       int       `json:"rank"`
	// Leg ranks (1-based; 0 = not found by that leg), for evaluation (014).
	VectorRank int `json:"vector_rank,omitempty"`
	TextRank   int `json:"text_rank,omitempty"`
}

// Retriever is the search interface generation depends on (RET-US-3).
type Retriever interface {
	Search(ctx context.Context, tenantID uuid.UUID, question string, limit int) ([]Hit, error)
}

// Options tunes the hybrid retriever; zero values use the defaults.
type Options struct {
	CandidatesPerLeg int
	K                float64
	PerDocumentCap   int
	// FallbackEfSearch is hnsw.ef_search when pgvector has no iterative
	// index scans (< 0.8): a wider candidate list so filtering by tenant
	// leaves enough rows for small tenants (RET-REQ-007).
	FallbackEfSearch int
}

func (o Options) withDefaults() Options {
	if o.CandidatesPerLeg <= 0 {
		o.CandidatesPerLeg = DefaultCandidatesPerLeg
	}
	if o.K <= 0 {
		o.K = DefaultK
	}
	if o.PerDocumentCap <= 0 {
		o.PerDocumentCap = DefaultPerDocumentCap
	}
	if o.FallbackEfSearch <= 0 {
		o.FallbackEfSearch = 400
	}
	return o
}

// Hybrid is the production Retriever.
type Hybrid struct {
	store    *storage.Store
	embedder llm.EmbeddingProvider
	opts     Options
	log      *slog.Logger

	mu        sync.Mutex
	detected  bool
	iterative bool // pgvector >= 0.8: hnsw.iterative_scan available
}

// NewHybrid builds the retriever. The embedder must be the one ingestion
// used (same model and dimension), or vectors are not comparable.
func NewHybrid(store *storage.Store, embedder llm.EmbeddingProvider, opts Options, log *slog.Logger) *Hybrid {
	if log == nil {
		log = slog.Default()
	}
	return &Hybrid{store: store, embedder: embedder, opts: opts.withDefaults(), log: log}
}

// ValidateQuestion trims the question and rejects empty or oversized input
// before any provider is called (RET-AC-006).
func ValidateQuestion(q string) (string, error) {
	q = strings.TrimSpace(q)
	switch {
	case q == "":
		return "", ErrEmptyQuestion
	case !utf8.ValidString(q):
		return "", ErrEmptyQuestion
	case utf8.RuneCountInString(q) > MaxQuestionRunes:
		return "", ErrQuestionTooLong
	}
	return q, nil
}

// Search implements Retriever. limit <= 0 uses DefaultLimit; values above
// MaxLimit are clamped (RET-AC-008 returns every valid candidate).
func (h *Hybrid) Search(ctx context.Context, tenantID uuid.UUID, question string, limit int) ([]Hit, error) {
	q, err := ValidateQuestion(question)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, 2*h.opts.CandidatesPerLeg)

	vecs, err := h.embedder.Embed(ctx, []string{q})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEmbeddingFailure, err)
	}
	if len(vecs) != 1 || len(vecs[0]) != llm.EmbeddingDim {
		return nil, fmt.Errorf("%w: got %d vectors, want 1 of %d dims", ErrEmbeddingFailure, len(vecs), llm.EmbeddingDim)
	}
	qvec := storage.VectorLiteral(vecs[0])

	var cands []Hit
	err = h.store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if err := h.tuneScan(ctx, tx); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, hybridSQL, tenantID, qvec, q, h.opts.CandidatesPerLeg)
		if err != nil {
			return err
		}
		cands, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Hit, error) {
			var c Hit
			err := r.Scan(&c.ChunkID, &c.DocumentID, &c.Filename, &c.Content, &c.Page, &c.Heading,
				&c.VectorRank, &c.TextRank)
			return c, err
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("retrieval: search: %w", err)
	}
	return Fuse(cands, h.opts.K, h.opts.PerDocumentCap, limit), nil
}

// tuneScan sets the HNSW scan options for this transaction only.
func (h *Hybrid) tuneScan(ctx context.Context, tx pgx.Tx) error {
	iterative, err := h.iterativeScan(ctx, tx)
	if err != nil {
		return err
	}
	// ef_search must be at least the leg LIMIT (the default 40 is below 50).
	ef := max(2*h.opts.CandidatesPerLeg, 100)
	if iterative {
		if _, err := tx.Exec(ctx, `SELECT set_config('hnsw.iterative_scan', 'relaxed_order', true)`); err != nil {
			return err
		}
	} else {
		ef = max(ef, h.opts.FallbackEfSearch)
	}
	_, err = tx.Exec(ctx, `SELECT set_config('hnsw.ef_search', $1, true)`, strconv.Itoa(min(ef, 1000)))
	return err
}

// iterativeScan detects once whether pgvector supports iterative index
// scans; a failed detection is retried on the next search.
func (h *Hybrid) iterativeScan(ctx context.Context, tx pgx.Tx) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.detected {
		return h.iterative, nil
	}
	var v string
	if err := tx.QueryRow(ctx, `SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&v); err != nil {
		return false, fmt.Errorf("pgvector version: %w", err)
	}
	h.iterative, h.detected = versionAtLeast(v, 0, 8), true
	h.log.Info("retrieval: pgvector", "version", v, "iterative_scan", h.iterative)
	return h.iterative, nil
}

// versionAtLeast compares "major.minor[.patch]" versions.
func versionAtLeast(v string, major, minor int) bool {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return ma > major || (ma == major && mi >= minor)
}
