package conversations

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/observability"
	"github.com/kavish924/multi-tenant-rag/internal/reranking"
	"github.com/kavish924/multi-tenant-rag/internal/tenants"
)

// OBS-AC-001 (ask half; retrieval spans are asserted in retrieval's
// integration suite) and OBS-AC-004: one trace with nested llm and rerank
// spans, and logs that carry correlation ids but no document text.
func TestAskTelemetry(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer otel.SetTracerProvider(prev)

	const canary = "CANARY-7f3a91"
	var logs bytes.Buffer
	src := hits(canary+" refund window is thirty days.", canary+" second passage.")
	h := &Handler{
		Retriever: fakeRetriever{hits: src},
		Generator: llm.FakeGenerator{},
		Rerank:    &reranking.Stage{Enabled: true, Reranker: &reverser{}},
		Log:       observability.NewLogger(&logs, nil),
	}
	tenant := testTenant
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ask", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(tenants.WithMembership(r.Context(), tenants.Membership{TenantID: tenant, Role: tenants.RoleMember}))
		h.Ask(w, r)
	})
	srv := httptest.NewServer(observability.Middleware(mux))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/ask", "application/json", strings.NewReader(`{"question":"`+canary+` question?"}`))
	if err != nil {
		t.Fatal(err)
	}
	evs := readEvents(t, res.Body)
	res.Body.Close()
	if kinds(evs) != "token,citation,done" {
		t.Fatalf("events %s", kinds(evs))
	}

	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range rec.Ended() {
		byName[s.Name()] = s
	}
	root := byName["POST /ask"]
	if root == nil {
		t.Fatalf("no server span; got %v", byName)
	}
	parentOf := map[string]string{"rerank": "POST /ask", "llm total": "POST /ask", "llm first-token": "llm total"}
	for child, parent := range parentOf {
		c, p := byName[child], byName[parent]
		if c == nil || p == nil {
			t.Errorf("missing span %s or %s", child, parent)
			continue
		}
		if c.SpanContext().TraceID() != root.SpanContext().TraceID() || c.Parent().SpanID() != p.SpanContext().SpanID() {
			t.Errorf("%s is not a child of %s in one trace", child, parent)
		}
	}
	if ft, tot := byName["llm first-token"], byName["llm total"]; ft != nil && tot != nil && ft.EndTime().After(tot.EndTime()) {
		t.Error("first-token ends after total")
	}

	out := logs.String()
	if strings.Contains(out, canary) {
		t.Errorf("document or question text reached the logs:\n%s", out)
	}
	if !strings.Contains(out, `"request_id":"`+res.Header.Get(observability.HeaderRequestID)+`"`) ||
		!strings.Contains(out, `"logging.googleapis.com/trace":"`+root.SpanContext().TraceID().String()) {
		t.Errorf("logs lack correlation ids:\n%s", out)
	}
}
