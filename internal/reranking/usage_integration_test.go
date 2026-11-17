//go:build integration

package reranking

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/testutil/pgtest"
	"github.com/kavish924/multi-tenant-rag/internal/usage"
)

var env *pgtest.Env

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	e, err := pgtest.Start(ctx)
	cancel()
	if err != nil {
		log.Fatalf("pgtest: %v", err)
	}
	env = e
	code := m.Run()
	env.Close()
	os.Exit(code)
}

// RER-AC-004: every rerank call, successful or not, writes a rerank row.
func TestRerankUsageRows(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)
	rec := usage.NewRecorder(env.Store)
	ok := &LLM{Generator: llm.FakeGenerator{Respond: func(llm.GenerateRequest) string { return "2, 1" }}, Usage: rec}
	if _, err := ok.Rerank(context.Background(), tid, "q", cands(3), 2); err != nil {
		t.Fatal(err)
	}
	bad := &LLM{Generator: llm.FakeGenerator{Respond: func(llm.GenerateRequest) string { return "no idea" }}, Usage: rec}
	if _, err := bad.Rerank(context.Background(), tid, "q", cands(3), 2); !errors.Is(err, ErrMalformed) {
		t.Fatal(err)
	}
	var okRows, errRows int
	_ = env.Admin.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE outcome = 'ok' AND input_tokens > 0 AND output_tokens > 0),
		       count(*) FILTER (WHERE outcome = 'error')
		FROM usage_events WHERE tenant_id = $1 AND kind = 'rerank' AND model = 'fake-generator-v1'`, tid).Scan(&okRows, &errRows)
	if okRows != 1 || errRows != 1 {
		t.Errorf("rerank usage rows ok=%d error=%d, want 1 and 1", okRows, errRows)
	}
}
