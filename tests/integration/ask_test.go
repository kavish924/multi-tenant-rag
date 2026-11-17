//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kavish924/multi-tenant-rag/internal/ingestion"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/embed"
	"github.com/kavish924/multi-tenant-rag/internal/ingestion/extract"
	"github.com/kavish924/multi-tenant-rag/internal/jobs"
	"github.com/kavish924/multi-tenant-rag/internal/llm"
)

// inProcess extracts without the helper process (this test binary has no
// helper entry point); isolation itself is covered by spec 005's tests.
type inProcess struct{}

func (inProcess) Extract(_ context.Context, ctype string, r io.Reader) ([]extract.Section, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return extract.Extract(ctype, b, extract.DefaultLimits)
}

// ingestAll runs every queued ingest job, as the worker would.
func ingestAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	p := &ingestion.Pipeline{Objects: objects, Extractor: inProcess{}, Embedder: &embed.Batcher{Provider: llm.Fake{}}}
	q := func() *jobs.Queue { q := jobs.NewQueue(env.Store, 0); q.SetTenantCap(1 << 20); return q }()
	for {
		j, err := q.Claim(ctx, "test", []string{ingestion.JobKind})
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			return
		}
		if err := p.Handle(ctx, *j, jobs.StoreScoper(env.Store)(j.TenantID)); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if err := q.Complete(ctx, *j, "test"); err != nil {
			t.Fatal(err)
		}
	}
}

type event struct {
	name string
	data map[string]any
}

func askSSE(t *testing.T, path, token, question string) (int, []event) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"question": question})
	req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Type") != "text/event-stream" {
		return res.StatusCode, nil
	}
	var evs []event
	sc := bufio.NewScanner(res.Body)
	var name string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Fatalf("bad data %q", line)
			}
			evs = append(evs, event{name, m})
		}
	}
	return res.StatusCode, evs
}

// End to end: upload → ingest → ask → cited answer, usage rows, and
// citation dereference re-checks access (GEN-AC-001/002/007/009).
func TestAskEndToEnd(t *testing.T) {
	env.Truncate(t)
	_, owner := user(t, "o@a.test")
	tid := createTenant(t, owner, "A")
	r := upload(t, owner, "", "refunds.txt", "text/plain", []byte(textBody), nil)
	docID := docOf(r)["id"].(string)
	ingestAll(t)

	for _, path := range []string{"/ask", "/tenants/" + tid + "/ask"} {
		status, evs := askSSE(t, path, owner, "How long do customers have to return items?")
		if status != http.StatusOK || len(evs) < 3 {
			t.Fatalf("%s: status %d events %v", path, status, evs)
		}
		var text strings.Builder
		var cites []map[string]any
		for _, e := range evs {
			switch e.name {
			case "token":
				text.WriteString(e.data["t"].(string))
			case "citation":
				cites = append(cites, e.data)
			case "error":
				t.Fatalf("error event %v", e.data)
			}
		}
		if evs[len(evs)-1].name != "done" || !strings.Contains(text.String(), "30 days") {
			t.Errorf("%s: answer %q, last %s", path, text.String(), evs[len(evs)-1].name)
		}
		if len(cites) != 1 || cites[0]["document_id"] != docID || cites[0]["n"] != float64(1) {
			t.Errorf("%s: citations %v", path, cites)
		}
	}

	if n := count(t, `SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND kind = 'generate' AND outcome = 'ok' AND input_tokens > 0 AND output_tokens > 0`, tid); n != 2 {
		t.Errorf("generate usage rows = %d, want 2", n)
	}
	if n := count(t, `SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND kind = 'embed'`, tid); n != 2 {
		t.Errorf("embed usage rows = %d, want 2", n)
	}

	// GEN-AC-009: dereferencing the citation re-checks access.
	expect(t, call(t, "GET", "/documents/"+docID+"/content", owner, nil), http.StatusOK, "citation dereference")
	_, other := user(t, "x@b.test")
	createTenant(t, other, "B")
	expect(t, call(t, "GET", "/documents/"+docID+"/content", other, nil), http.StatusNotFound, "other tenant dereference")
	expect(t, call(t, "DELETE", "/documents/"+docID, owner, nil), http.StatusAccepted, "delete")
	expect(t, call(t, "GET", "/documents/"+docID+"/content", owner, nil), http.StatusGone, "deleted dereference")

	// Deleted content is no longer retrievable, so the answer has no sources.
	_, evs := askSSE(t, "/ask", owner, "How long do customers have to return items?")
	last := evs[len(evs)-1]
	if last.name != "done" || last.data["insufficient_context"] != true {
		t.Errorf("after delete: %v", evs)
	}

	// Tenant B asking the same question sees nothing of tenant A.
	_, evs = askSSE(t, "/ask", other, "How long do customers have to return items?")
	for _, e := range evs {
		if e.name == "citation" {
			t.Errorf("tenant B got a citation: %v", e.data)
		}
	}
	if n := count(t, `SELECT count(*) FROM usage_events WHERE tenant_id <> $1`, tid); n != 1 {
		t.Errorf("tenant B usage rows = %d, want 1 (embed only)", n)
	}
}

func TestAskValidation(t *testing.T) {
	env.Truncate(t)
	_, owner := user(t, "o@a.test")
	createTenant(t, owner, "A")
	if status, _ := askSSE(t, "/ask", owner, "  "); status != http.StatusBadRequest {
		t.Errorf("blank question: %d", status)
	}
	if status, _ := askSSE(t, "/ask", "", "q"); status != http.StatusUnauthorized {
		t.Errorf("no token: %d", status)
	}
	if status, _ := askSSE(t, "/tenants/"+uuid.New().String()+"/ask", owner, "q"); status != http.StatusForbidden && status != http.StatusNotFound {
		t.Errorf("foreign tenant path: %d", status)
	}
}
