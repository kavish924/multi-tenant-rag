// Package embed batches chunk texts through an EmbeddingProvider, validates
// every vector's dimension (ING-REQ-005) and retries provider rate limits a
// bounded number of times (ING-REQ-007).
package embed

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/observability"
)

// ErrDimensionMismatch is permanent: the configured model and the schema
// disagree, so retrying cannot help (ING-AC-003).
var ErrDimensionMismatch = errors.New("embedding_dimension_mismatch")

// Batcher embeds texts in fixed-size batches.
type Batcher struct {
	Provider  llm.EmbeddingProvider
	BatchSize int           // default 32
	Retries   int           // rate-limit retries per batch, default 3
	Backoff   time.Duration // first retry delay, doubled each time; default 1s
	// Dim is the vector length the schema stores; default llm.EmbeddingDim.
	Dim int
}

// Embed returns one vector per text, in order. Nothing is returned (and so
// nothing can be written) unless every vector has the right dimension.
func (b *Batcher) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	size := b.BatchSize
	if size <= 0 {
		size = 32
	}
	dim := b.Dim
	if dim <= 0 {
		dim = llm.EmbeddingDim
	}
	if b.Provider.Dim() != dim {
		return nil, fmt.Errorf("%w: provider %s returns %d dims, schema stores %d",
			ErrDimensionMismatch, b.Provider.Model(), b.Provider.Dim(), dim)
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += size {
		batch := texts[start:min(start+size, len(texts))]
		bctx, span := observability.Start(ctx, "embed batch",
			observability.AttrProvider.String(b.Provider.Model()), attribute.Int("batch.size", len(batch)))
		vecs, err := b.embedBatch(bctx, batch)
		observability.End(span, err)
		if err != nil {
			observability.M().ProviderError(ctx, b.Provider.Model(), "embed")
			return nil, err
		}
		if len(vecs) != len(batch) {
			return nil, fmt.Errorf("embed: provider returned %d vectors for %d texts", len(vecs), len(batch))
		}
		for i, v := range vecs {
			if len(v) != dim {
				return nil, fmt.Errorf("%w: vector %d has %d dims, want %d", ErrDimensionMismatch, start+i, len(v), dim)
			}
			for _, x := range v {
				if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
					return nil, fmt.Errorf("embed: vector %d contains NaN or Inf", start+i)
				}
			}
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (b *Batcher) embedBatch(ctx context.Context, batch []string) ([][]float32, error) {
	retries := b.Retries
	if retries <= 0 {
		retries = 3
	}
	delay := b.Backoff
	if delay <= 0 {
		delay = time.Second
	}
	for attempt := 0; ; attempt++ {
		vecs, err := b.Provider.Embed(ctx, batch)
		if err == nil {
			return vecs, nil
		}
		if !errors.Is(err, llm.ErrRateLimited) || attempt >= retries {
			// Above this, the job queue's own backoff takes over.
			return nil, fmt.Errorf("embed: %w", err)
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
		delay *= 2
	}
}
