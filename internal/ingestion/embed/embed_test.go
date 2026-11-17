package embed

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/kavish924/multi-tenant-rag/internal/llm"
)

type scripted struct {
	dim       int
	calls     int
	rateLimit int // first N calls return ErrRateLimited
	badAt     int // 1-based call whose first vector has the wrong size (0 = never)
	batches   []int
}

func (s *scripted) Model() string { return "scripted" }
func (s *scripted) Dim() int      { return s.dim }
func (s *scripted) Embed(_ context.Context, texts []string) ([][]float32, error) {
	s.calls++
	if s.calls <= s.rateLimit {
		return nil, fmt.Errorf("429: %w", llm.ErrRateLimited)
	}
	s.batches = append(s.batches, len(texts))
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, s.dim)
	}
	if s.calls == s.badAt {
		out[0] = out[0][:s.dim-1]
	}
	return out, nil
}

func texts(n int) []string {
	t := make([]string, n)
	for i := range t {
		t[i] = fmt.Sprint("text ", i)
	}
	return t
}

func TestBatching(t *testing.T) {
	p := &scripted{dim: 8}
	b := Batcher{Provider: p, BatchSize: 4, Dim: 8}
	v, err := b.Embed(context.Background(), texts(10))
	if err != nil || len(v) != 10 {
		t.Fatalf("%d %v", len(v), err)
	}
	if fmt.Sprint(p.batches) != "[4 4 2]" {
		t.Errorf("batches %v", p.batches)
	}
}

// ING-AC-003.
func TestDimensionMismatch(t *testing.T) {
	b := Batcher{Provider: &scripted{dim: 8, badAt: 2}, BatchSize: 4, Dim: 8}
	if _, err := b.Embed(context.Background(), texts(10)); !errors.Is(err, ErrDimensionMismatch) {
		t.Errorf("bad vector: %v", err)
	}
	b = Batcher{Provider: &scripted{dim: 16}, Dim: 8}
	if _, err := b.Embed(context.Background(), texts(1)); !errors.Is(err, ErrDimensionMismatch) {
		t.Errorf("configured dim mismatch: %v", err)
	}
}

// ING-REQ-007.
func TestRateLimitRetries(t *testing.T) {
	p := &scripted{dim: 8, rateLimit: 2}
	b := Batcher{Provider: p, Dim: 8, Retries: 3, Backoff: 1}
	if _, err := b.Embed(context.Background(), texts(3)); err != nil {
		t.Fatalf("recovered rate limit: %v", err)
	}
	p = &scripted{dim: 8, rateLimit: 10}
	b = Batcher{Provider: p, Dim: 8, Retries: 2, Backoff: 1}
	if _, err := b.Embed(context.Background(), texts(3)); !errors.Is(err, llm.ErrRateLimited) {
		t.Fatalf("exhausted retries: %v", err)
	}
	if p.calls != 3 {
		t.Errorf("calls %d, want 3 (1 + 2 retries)", p.calls)
	}
}

func TestFakeProvider(t *testing.T) {
	f := llm.Fake{}
	b := Batcher{Provider: f}
	v, err := b.Embed(context.Background(), []string{"refund policy thirty days", "refund policy thirty days", "quarterly revenue report"})
	if err != nil {
		t.Fatal(err)
	}
	cos := func(a, b []float32) float64 {
		var s float64
		for i := range a {
			s += float64(a[i]) * float64(b[i])
		}
		return s
	}
	if math.Abs(cos(v[0], v[1])-1) > 1e-5 {
		t.Error("fake embedder is not deterministic")
	}
	if cos(v[0], v[2]) > 0.5 {
		t.Errorf("unrelated texts too similar: %.2f", cos(v[0], v[2]))
	}
	if math.Abs(cos(v[0], v[0])-1) > 1e-5 {
		t.Error("not unit length")
	}
}
