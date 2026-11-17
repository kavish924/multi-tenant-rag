package breaker

import (
	"context"
	"errors"
	"io"

	"github.com/kavish924/multi-tenant-rag/internal/llm"
)

// Embedder wraps an embedding provider with a breaker. Ingestion sees
// ErrOpen as a retryable error, so the job retry ladder is unaffected
// (QTA-REQ-005).
type Embedder struct {
	llm.EmbeddingProvider
	B *Breaker
}

// Embed implements llm.EmbeddingProvider.
func (e Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := e.B.Allow(); err != nil {
		return nil, err
	}
	v, err := e.EmbeddingProvider.Embed(ctx, texts)
	e.B.Done(err)
	return v, err
}

// Available reports whether the provider's circuit admits calls.
func (e Embedder) Available() error { return e.B.Available() }

// Generator wraps a generator with a breaker. A stream counts as a success
// when it reaches io.EOF and as a failure on any other terminal error.
type Generator struct {
	llm.Generator
	B *Breaker
}

// Stream implements llm.Generator.
func (g Generator) Stream(ctx context.Context, req llm.GenerateRequest) (llm.Stream, error) {
	if err := g.B.Allow(); err != nil {
		return nil, err
	}
	s, err := g.Generator.Stream(ctx, req)
	if err != nil {
		g.B.Done(err)
		return nil, err
	}
	return &stream{Stream: s, b: g.B}, nil
}

// Available reports whether the provider's circuit admits calls.
func (g Generator) Available() error { return g.B.Available() }

type stream struct {
	llm.Stream
	b    *Breaker
	done bool
}

func (s *stream) Recv() (string, error) {
	tok, err := s.Stream.Recv()
	if err != nil && !s.done {
		s.done = true
		if errors.Is(err, io.EOF) {
			s.b.Done(nil)
		} else {
			s.b.Done(err)
		}
	}
	return tok, err
}

func (s *stream) Close() error {
	if !s.done {
		s.done = true
		s.b.Done(context.Canceled) // abandoned early: neither outcome
	}
	return s.Stream.Close()
}
