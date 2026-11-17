//go:build integration

package retrieval_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
	"github.com/kavish924/multi-tenant-rag/internal/testutil/pgtest"
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

// concepts is a toy "semantic" embedder: words map to concept axes, so
// synonyms share a direction while sharing no lexemes. That isolates the
// vector leg from the full-text leg (RET-AC-005).
type concepts struct{}

var conceptOf = map[string]int{
	"vacation": 1, "holiday": 1, "leave": 1, "pto": 1,
	"salary": 2, "pay": 2, "compensation": 2, "wages": 2,
	"laptop": 3, "computer": 3, "hardware": 3,
	"refund": 4, "reimbursement": 4,
}

func (concepts) Model() string { return "concepts-test" }
func (concepts) Dim() int      { return llm.EmbeddingDim }
func (c concepts) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, llm.EmbeddingDim)
		v[0] = 0.05 // never a zero vector
		for _, w := range strings.FieldsFunc(strings.ToLower(t), func(r rune) bool { return r == ' ' || r == '.' || r == ',' || r == '?' }) {
			if k, ok := conceptOf[w]; ok {
				v[k]++
			}
		}
		var n float64
		for _, x := range v {
			n += float64(x * x)
		}
		for j := range v {
			v[j] /= float32(math.Sqrt(n))
		}
		out[i] = v
	}
	return out, nil
}

func newRetriever() *retrieval.Hybrid {
	return retrieval.NewHybrid(env.Store, concepts{}, retrieval.Options{}, nil)
}

func seedDoc(t *testing.T, tenant uuid.UUID, name string, chunks ...string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	sum := sha256.Sum256([]byte(id.String()))
	if _, err := env.Admin.Exec(ctx, `
		INSERT INTO documents (id, tenant_id, filename, content_type, size_bytes, object_key, sha256, status)
		VALUES ($1, $2, $3, 'text/plain', 1, 'tenants/' || $2::text || '/documents/' || $1::text, $4, 'indexed')`,
		id, tenant, name, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	vecs, _ := concepts{}.Embed(ctx, chunks)
	for i, c := range chunks {
		if _, err := env.Admin.Exec(ctx, `
			INSERT INTO chunks (tenant_id, document_id, ordinal, content, token_count, page, heading, embed_model, embedding)
			VALUES ($1, $2, $3, $4, 10, 1, 'Policy', 'concepts-test', $5::vector)`,
			tenant, id, i, c, storage.VectorLiteral(vecs[i])); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func search(t *testing.T, tenant uuid.UUID, q string, limit int) []retrieval.Hit {
	t.Helper()
	hits, err := newRetriever().Search(context.Background(), tenant, q, limit)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func rankOf(hits []retrieval.Hit, doc uuid.UUID) int {
	for _, h := range hits {
		if h.DocumentID == doc {
			return h.Rank
		}
	}
	return 0
}

func noise(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("General office note %d about meetings, parking and the cafeteria menu.", i)
	}
	return out
}

// RET-AC-004: an exact identifier is found by the full-text leg.
func TestLexicalHit(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)
	seedDoc(t, tid, "noise.txt", noise(30)...)
	target := seedDoc(t, tid, "errors.txt", "Gateway returns ERR-4012 when the upstream certificate has expired.")

	hits := search(t, tid, "ERR-4012", 10)
	r := rankOf(hits, target)
	if r == 0 || r > 10 {
		t.Fatalf("ERR-4012 chunk rank %d in %d hits", r, len(hits))
	}
	if hits[r-1].TextRank != 1 || hits[r-1].Filename != "errors.txt" || hits[r-1].Page != 1 || hits[r-1].Heading != "Policy" {
		t.Errorf("hit %+v", hits[r-1])
	}
}

// RET-AC-005: a synonym question is found by the vector leg alone.
func TestSemanticHit(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)
	seedDoc(t, tid, "noise.txt", noise(30)...)
	seedDoc(t, tid, "it.txt", "Every new starter receives a laptop on day one.")
	target := seedDoc(t, tid, "hr.txt", "Staff accrue twenty-five days of annual leave.")

	hits := search(t, tid, "How much vacation do I get?", 10)
	r := rankOf(hits, target)
	if r == 0 || r > 10 {
		t.Fatalf("leave chunk rank %d", r)
	}
	if h := hits[r-1]; h.TextRank != 0 || h.VectorRank == 0 {
		t.Errorf("expected a vector-only hit, got vector %d text %d", h.VectorRank, h.TextRank)
	}
}

// RET-AC-002: soft-deleted (deleting) documents never appear.
func TestDeletedExcluded(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)
	gone := seedDoc(t, tid, "old.txt", "Refund requests need a receipt ERR-77.")
	kept := seedDoc(t, tid, "new.txt", "Refund requests need the order number ERR-77.")
	if _, err := env.Admin.Exec(context.Background(),
		`UPDATE documents SET deleted_at = now(), status = 'deleting' WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	hits := search(t, tid, "refund ERR-77", 50)
	if rankOf(hits, gone) != 0 {
		t.Error("deleted document returned")
	}
	if rankOf(hits, kept) == 0 {
		t.Error("live document missing")
	}
}

// RET-AC-001: only the caller's tenant, including under direct id probes.
func TestCrossTenant(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	a, b := env.SeedTenant(t, "A", nil), env.SeedTenant(t, "B", nil)
	seedDoc(t, a, "a.txt", "Tenant A salary bands ERR-1.")
	bDoc := seedDoc(t, b, "b.txt", "Tenant B salary bands ERR-1.", "Tenant B secret compensation ERR-1.")

	hits := search(t, a, "salary ERR-1", 50)
	if len(hits) != 1 || rankOf(hits, bDoc) != 0 {
		t.Fatalf("tenant A saw %d hits: %+v", len(hits), hits)
	}
	// The row's own tenant and deleted flag are returned for the hygiene guard (012).
	if hits[0].TenantID != a || hits[0].Deleted {
		t.Errorf("hit tenant %s deleted %v, want %s false", hits[0].TenantID, hits[0].Deleted, a)
	}
	// Tenant B's chunk ids, probed directly inside tenant A's context.
	rows, _ := env.Admin.Query(ctx, `SELECT id FROM chunks WHERE tenant_id = $1`, b)
	ids, _ := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	var n int
	err := env.Store.WithTenant(ctx, a, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM chunks WHERE id = ANY($1)`, ids).Scan(&n)
	})
	if err != nil || n != 0 || len(ids) != 2 {
		t.Errorf("probe saw %d of %d foreign chunks (err %v)", n, len(ids), err)
	}
	// Searching "as" tenant B through tenant A's id is impossible: the
	// tenant id is the only scope, and an unknown tenant sees nothing.
	if hits := search(t, uuid.New(), "salary ERR-1", 50); len(hits) != 0 {
		t.Errorf("unknown tenant saw %d hits", len(hits))
	}
}

// RET-AC-003: one dominant document is capped.
func TestPerDocumentCap(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)
	var many []string
	for i := 0; i < 20; i++ {
		many = append(many, fmt.Sprintf("Section %d: refund reimbursement rules for case %d.", i, i))
	}
	big := seedDoc(t, tid, "big.txt", many...)
	other := seedDoc(t, tid, "other.txt", "Refund reimbursement for travel.")

	hits := search(t, tid, "refund reimbursement", 50)
	n := 0
	for _, h := range hits {
		if h.DocumentID == big {
			n++
		}
	}
	if n != retrieval.DefaultPerDocumentCap || rankOf(hits, other) == 0 {
		t.Errorf("big doc hits %d (cap %d), other rank %d", n, retrieval.DefaultPerDocumentCap, rankOf(hits, other))
	}
}

// RET-AC-008: a limit above the pool returns every candidate, by score.
func TestLimitAbovePool(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)
	seedDoc(t, tid, "a.txt", "Laptop policy.", "Hardware refresh every three years.")
	seedDoc(t, tid, "b.txt", "Computer accessories need approval.")

	hits := search(t, tid, "laptop hardware", 100)
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want all 3", len(hits))
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score || hits[i].Rank != i+1 {
			t.Errorf("order/rank at %d: %+v", i, hits)
		}
	}
	// The vector leg always returns the nearest chunks, so even an unrelated
	// question yields hits; judging relevance is generation's job (008).
}

// RET-REQ-007: a small tenant next to a large one still gets its chunks
// from the vector leg (HNSW post-filtering would otherwise starve it).
func TestSmallTenantRecall(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	big, small := env.SeedTenant(t, "Big", nil), env.SeedTenant(t, "Small", nil)
	bigDoc := seedDoc(t, big, "bulk.txt", "seed")
	if _, err := env.Admin.Exec(ctx, `
		INSERT INTO chunks (tenant_id, document_id, ordinal, content, token_count, embed_model, embedding)
		SELECT $1, $2, g, 'salary pay compensation wages ' || g, 5, 'concepts-test',
		       (SELECT ('[' || string_agg(CASE WHEN i = 2 THEN '1' ELSE (random() * 0.01 + g * 0)::text END, ',' ORDER BY i) || ']')::vector
		        FROM generate_series(0, 767) i)
		FROM generate_series(1, 3000) g`, big, bigDoc); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Admin.Exec(ctx, `ANALYZE chunks`); err != nil {
		t.Fatal(err)
	}
	want := seedDoc(t, small, "pay.txt", "Payroll runs monthly.", "Our wages are reviewed yearly.", "Bonus compensation is discretionary.")

	hits := search(t, small, "salary", 10)
	n := 0
	for _, h := range hits {
		if h.DocumentID == want && h.VectorRank > 0 {
			n++
		}
	}
	if n != 3 {
		t.Errorf("small tenant got %d of 3 vector hits: %+v", n, hits)
	}
}

// OBS-AC-001 (retrieval half): one retrieve span with the embed, vector,
// full-text, hydrate and fusion children.
func TestSearchSpans(t *testing.T) {
	env.Truncate(t)
	tid := env.SeedTenant(t, "A", nil)
	seedDoc(t, tid, "a.txt", "Refund policy ERR-9.")
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	defer otel.SetTracerProvider(prev)

	search(t, tid, "refund ERR-9", 8)
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range rec.Ended() {
		byName[s.Name()] = s
	}
	root := byName["retrieve"]
	if root == nil {
		t.Fatalf("no retrieve span: %v", byName)
	}
	for _, name := range []string{"embed question", "vector search", "full-text search", "hydrate candidates", "fusion"} {
		s := byName[name]
		if s == nil || s.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("span %q missing or not under retrieve", name)
		}
	}
}
