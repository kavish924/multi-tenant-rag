package conversations

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kavish924/multi-tenant-rag/internal/breaker"
	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
)

type countingRetriever struct {
	calls int
	err   error
	hits  []retrieval.Hit
}

func (c *countingRetriever) Search(context.Context, uuid.UUID, string, int) ([]retrieval.Hit, error) {
	c.calls++
	return c.hits, c.err
}

// QTA-AC-006: an open generation circuit fails fast with 503 before
// retrieval; an open embedding circuit (surfacing from retrieval) is 503
// too, not 502.
func TestAskProviderCircuitOpen(t *testing.T) {
	b := breaker.New("generate", 1, time.Minute)
	_ = b.Do(func() error { return errors.New("down") })
	ret := &countingRetriever{hits: hits("x")}
	srv := server(&Handler{Retriever: ret, Generator: breaker.Generator{Generator: llm.FakeGenerator{}, B: b}})
	defer srv.Close()
	resp, _ := ask(t, srv, "q")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" || ret.calls != 0 {
		t.Errorf("status %d, retrieval calls %d", resp.StatusCode, ret.calls)
	}

	ret2 := &countingRetriever{err: errors.Join(retrieval.ErrEmbeddingFailure, breaker.ErrOpen)}
	srv2 := server(&Handler{Retriever: ret2, Generator: llm.FakeGenerator{}})
	defer srv2.Close()
	if resp, _ := ask(t, srv2, "q"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("embed circuit open: status %d", resp.StatusCode)
	}
}
