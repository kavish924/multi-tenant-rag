// Package llm holds the provider interfaces the platform depends on. The
// rest of the code never imports a vendor SDK (docs/tech-stack.md).
package llm

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// EmbeddingDim is the vector size stored in chunks.embedding (migration
// 0006). Every provider must return vectors of exactly this length.
const EmbeddingDim = 768

// ErrRateLimited marks provider back-pressure (HTTP 429 /
// RESOURCE_EXHAUSTED). Callers retry with backoff (ING-REQ-007).
var ErrRateLimited = errors.New("llm: rate limited")

// EmbeddingProvider turns texts into vectors, one per input, in order.
type EmbeddingProvider interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Model is recorded per chunk (embed_model) so a model change can be
	// re-embedded later (ING-REQ-006).
	Model() string
	// Dim is the vector length the provider is configured to return.
	Dim() int
}

// Fake is a deterministic, dependency-free embedder for development and
// tests. It hashes word unigrams and bigrams into a fixed number of buckets
// and L2-normalizes, so texts sharing words have higher cosine similarity.
// It is not a semantic model.
type Fake struct {
	Dimension int // 0 = EmbeddingDim
}

// Model implements EmbeddingProvider.
func (f Fake) Model() string { return "fake-hash-v1" }

// Dim implements EmbeddingProvider.
func (f Fake) Dim() int {
	if f.Dimension > 0 {
		return f.Dimension
	}
	return EmbeddingDim
}

// Embed implements EmbeddingProvider.
func (f Fake) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out[i] = f.vector(t)
	}
	return out, nil
}

func (f Fake) vector(text string) []float32 {
	dim := f.Dim()
	v := make([]float64, dim)
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	add := func(feature string, weight float64) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(feature))
		sum := h.Sum64()
		sign := 1.0
		if sum&1 == 1 {
			sign = -1
		}
		v[(sum>>1)%uint64(dim)] += sign * weight //nolint:gosec // dim is a small positive int
	}
	for i, w := range words {
		add(w, 1)
		if i > 0 {
			add(words[i-1]+" "+w, 0.5)
		}
	}
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	out := make([]float32, dim)
	if norm == 0 {
		out[0] = 1 // a valid unit vector for empty text
		return out
	}
	norm = math.Sqrt(norm)
	for i, x := range v {
		out[i] = float32(x / norm)
	}
	return out
}
