package breaker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kavish924/multi-tenant-rag/internal/llm"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTest(threshold int, openFor time.Duration) (*Breaker, *clock) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	b := New("test", threshold, openFor)
	b.Now = c.now
	b.Log = slog.New(slog.DiscardHandler)
	return b, c
}

var boom = errors.New("503")

// 4.2: closed → open → half-open → closed, and half-open → open on a
// failed probe.
func TestTransitions(t *testing.T) {
	b, c := newTest(3, 10*time.Second)
	for i := 0; i < 2; i++ {
		if b.Do(func() error { return boom }) != boom || b.State() != Closed {
			t.Fatalf("failure %d opened early", i+1)
		}
	}
	_ = b.Do(func() error { return nil }) // a success resets the count
	for i := 0; i < 3; i++ {
		_ = b.Do(func() error { return boom })
	}
	if b.State() != Open {
		t.Fatalf("state %v after 3 failures", b.State())
	}
	// QTA-AC-006: open fails fast without calling the provider.
	called := false
	if err := b.Do(func() error { called = true; return nil }); !errors.Is(err, ErrOpen) || called || b.Available() == nil {
		t.Fatalf("open breaker let a call through: %v", err)
	}

	c.t = c.t.Add(10 * time.Second)
	if b.State() != HalfOpen || b.Available() != nil {
		t.Fatalf("state %v after open period", b.State())
	}
	// One probe at a time.
	if err := b.Allow(); err != nil {
		t.Fatal(err)
	}
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Error("second concurrent probe admitted")
	}
	b.Done(boom) // failed probe re-opens at once
	if b.State() != Open {
		t.Fatalf("failed probe left state %v", b.State())
	}
	c.t = c.t.Add(10 * time.Second)
	if err := b.Do(func() error { return nil }); err != nil || b.State() != Closed {
		t.Fatalf("successful probe: %v state %v", err, b.State())
	}
}

func TestCancellationIsNeutral(t *testing.T) {
	b, _ := newTest(1, time.Minute)
	_ = b.Do(func() error { return context.Canceled })
	if b.State() != Closed {
		t.Error("a caller cancellation opened the circuit")
	}
	_ = b.Do(func() error { return llm.ErrRateLimited }) // sustained 429s count
	if b.State() != Open {
		t.Error("429 did not count as a failure")
	}
}

type fakeEmbedder struct {
	llm.Fake
	err   error
	calls int
}

func (f *fakeEmbedder) Embed(ctx context.Context, t []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.Fake.Embed(ctx, t)
}

func TestEmbedderWrapper(t *testing.T) {
	b, _ := newTest(2, time.Minute)
	inner := &fakeEmbedder{err: boom}
	e := Embedder{EmbeddingProvider: inner, B: b}
	for i := 0; i < 2; i++ {
		_, _ = e.Embed(context.Background(), []string{"x"})
	}
	if _, err := e.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrOpen) || inner.calls != 2 {
		t.Errorf("err %v, provider calls %d", err, inner.calls)
	}
	if e.Model() != (llm.Fake{}).Model() || e.Available() == nil {
		t.Error("wrapper must expose the provider and its availability")
	}
}

func TestGeneratorWrapper(t *testing.T) {
	drain := func(s llm.Stream) error {
		for {
			if _, err := s.Recv(); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
	}
	b, _ := newTest(1, time.Minute)
	ok := Generator{Generator: llm.FakeGenerator{Respond: func(llm.GenerateRequest) string { return "a b c" }}, B: b}
	s, err := ok.Stream(context.Background(), llm.GenerateRequest{})
	if err != nil || drain(s) != nil || b.State() != Closed {
		t.Fatalf("clean stream: %v state %v", err, b.State())
	}

	// Abandoned early: neither outcome.
	s, _ = ok.Stream(context.Background(), llm.GenerateRequest{})
	_, _ = s.Recv()
	_ = s.Close()
	if b.State() != Closed {
		t.Error("early close counted as failure")
	}

	bad := Generator{Generator: llm.FakeGenerator{Respond: func(llm.GenerateRequest) string { return "a b c" }, FailAfter: 1}, B: b}
	s, _ = bad.Stream(context.Background(), llm.GenerateRequest{})
	if drain(s) == nil || b.State() != Open {
		t.Fatalf("mid-stream failure: state %v", b.State())
	}
	if _, err := ok.Stream(context.Background(), llm.GenerateRequest{}); !errors.Is(err, ErrOpen) {
		t.Errorf("open circuit started a stream: %v", err)
	}
}
