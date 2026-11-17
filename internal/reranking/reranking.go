// Package reranking is the optional stage between fusion and prompt build
// (spec 009): a cross-encoder or LLM reranker narrows the fused top-50 to
// the prompt's top-8. It is off by default and fails open: any error,
// timeout or malformed output falls back to the fused order.
package reranking

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"github.com/kavish924/multi-tenant-rag/internal/observability"
	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
)

// Defaults (RER-REQ-001).
const (
	DefaultKeep       = 8
	DefaultTimeout    = 800 * time.Millisecond
	DefaultCandidates = retrieval.DefaultCandidatesPerLeg
)

// Errors an implementation returns; all of them trigger the fallback.
var (
	ErrMalformed = errors.New("reranking: malformed reranker output")
	ErrInvalid   = errors.New("reranking: reranker returned hits that were not candidates")
)

// Reranker reorders candidate hits by relevance to the question and returns
// at most keep of them. Implementations must return hits from the input
// only; the Stage re-checks that.
type Reranker interface {
	Rerank(ctx context.Context, tenantID uuid.UUID, question string, hits []retrieval.Hit, keep int) ([]retrieval.Hit, error)
}

// Stage applies a Reranker with a timeout and fail-open fallback. A nil
// *Stage, or one with Enabled false, passes the first Keep hits through
// without calling any provider (RER-AC-001).
type Stage struct {
	Enabled  bool
	Reranker Reranker
	Keep     int           // zero = DefaultKeep
	Timeout  time.Duration // zero = DefaultTimeout
	// Candidates is how many fused hits retrieval should return when the
	// stage is enabled; zero = DefaultCandidates.
	Candidates int
	Log        *slog.Logger
	// OnFallback is the rerank_fallback metric hook (spec 010) with a
	// reason: "timeout", "error" or "invalid".
	OnFallback func(reason string)
}

// Active reports whether reranking runs.
func (s *Stage) Active() bool { return s != nil && s.Enabled && s.Reranker != nil }

// Want is the retrieval limit to request: the candidate pool when active,
// otherwise just the prompt's keep.
func (s *Stage) Want(keep int) int {
	if !s.Active() {
		return keep
	}
	if s.Candidates > 0 {
		return s.Candidates
	}
	return DefaultCandidates
}

// KeepOr is the number of hits the prompt gets.
func (s *Stage) KeepOr(def int) int {
	if s != nil && s.Keep > 0 {
		return s.Keep
	}
	return def
}

// Apply returns at most keep hits for the prompt. It never fails: on any
// reranker problem it returns the fused order (RER-REQ-003). Returned hits
// are the original candidate values (identity preserved, RER-REQ-005),
// renumbered from rank 1.
func (s *Stage) Apply(ctx context.Context, tenantID uuid.UUID, question string, hits []retrieval.Hit) []retrieval.Hit {
	keep := s.KeepOr(DefaultKeep)
	if !s.Active() || len(hits) == 0 {
		return firstN(hits, keep)
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, span := observability.Start(ctx, "rerank", observability.AttrTenantID.String(tenantID.String()))
	defer span.End()
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	out, err := s.Reranker.Rerank(rctx, tenantID, question, hits, keep)
	if err == nil {
		out, err = s.verify(hits, out, keep)
	}
	if err != nil {
		reason := "error"
		switch {
		case errors.Is(err, context.DeadlineExceeded) || rctx.Err() == context.DeadlineExceeded:
			reason = "timeout"
		case errors.Is(err, ErrMalformed), errors.Is(err, ErrInvalid):
			reason = "invalid"
		}
		span.SetAttributes(attribute.String("rerank.fallback", reason))
		observability.M().RerankFallback(ctx, reason)
		if reason != "invalid" {
			observability.M().ProviderError(ctx, "reranker", "rerank")
		}
		s.log().WarnContext(ctx, "rerank fallback to fused order", "tenant_id", tenantID, "reason", reason,
			"duration_ms", time.Since(start).Milliseconds(), "err", err)
		if s.OnFallback != nil {
			s.OnFallback(reason)
		}
		return firstN(hits, keep)
	}
	s.log().Debug("reranked", "tenant_id", tenantID, "candidates", len(hits), "kept", len(out),
		"duration_ms", time.Since(start).Milliseconds())
	return out
}

// verify maps the reranker's output back to the original candidates by
// chunk id, rejecting unknown or duplicate ids, and truncates to keep.
func (s *Stage) verify(cands, got []retrieval.Hit, keep int) ([]retrieval.Hit, error) {
	if len(got) == 0 {
		return nil, fmt.Errorf("%w: no hits", ErrInvalid)
	}
	byID := make(map[uuid.UUID]retrieval.Hit, len(cands))
	for _, h := range cands {
		byID[h.ChunkID] = h
	}
	seen := map[uuid.UUID]bool{}
	out := make([]retrieval.Hit, 0, min(keep, len(got)))
	for _, g := range got {
		orig, ok := byID[g.ChunkID]
		if !ok || seen[g.ChunkID] {
			return nil, fmt.Errorf("%w: chunk %s", ErrInvalid, g.ChunkID)
		}
		seen[g.ChunkID] = true
		if len(out) == keep {
			continue
		}
		orig.RerankScore = g.RerankScore
		orig.Rank = len(out) + 1
		out = append(out, orig)
	}
	return out, nil
}

func firstN(hits []retrieval.Hit, n int) []retrieval.Hit {
	if len(hits) <= n {
		return hits
	}
	return hits[:n]
}

func (s *Stage) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}
