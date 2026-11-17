package reranking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/google/uuid"

	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
	"github.com/kavish924/multi-tenant-rag/internal/usage"
)

// CrossEncoder calls a hosted scoring endpoint (RER-REQ-001, provider
// "cross_encoder"). The wire shape follows the Vertex AI Ranking API:
//
//	POST {"model", "query", "topN", "records": [{"id", "content"}]}
//	→    {"records": [{"id", "score"}]}
//
// Only this request's hits are sent (RER-REQ-006); record ids are the
// candidate positions, never database ids.
type CrossEncoder struct {
	Endpoint string
	Model    string
	Client   *http.Client // nil = http.DefaultClient
	// Authorize, when set, adds credentials to each request (an OAuth
	// token for Vertex AI; added with the GCP work).
	Authorize func(*http.Request) error
	Usage     *usage.Recorder
}

type ceRecord struct {
	ID      string  `json:"id"`
	Content string  `json:"content,omitempty"`
	Score   float64 `json:"score,omitempty"`
}

type ceRequest struct {
	Model   string     `json:"model,omitempty"`
	Query   string     `json:"query"`
	TopN    int        `json:"topN"`
	Records []ceRecord `json:"records"`
}

type ceResponse struct {
	Records []ceRecord `json:"records"`
}

// Rerank implements Reranker.
func (c *CrossEncoder) Rerank(ctx context.Context, tenantID uuid.UUID, question string, hits []retrieval.Hit, keep int) (out []retrieval.Hit, err error) {
	req := ceRequest{Model: c.Model, Query: question, TopN: min(keep, len(hits))}
	// Usage is attributed in approximate input tokens (query + records), the
	// unit quotas (011) sum; the provider bills per request.
	tokens := llm.ApproxTokens(question)
	for i, h := range hits {
		content := truncate(h.Content, 4*MaxPassageBytes)
		req.Records = append(req.Records, ceRecord{ID: fmt.Sprint(i), Content: content})
		tokens += llm.ApproxTokens(content)
	}
	defer func() {
		outcome := usage.OutcomeOK
		if err != nil {
			outcome = usage.OutcomeError
			if ctx.Err() != nil {
				outcome = usage.OutcomeCancelled
			}
		}
		model := c.Model
		if model == "" {
			model = "cross-encoder"
		}
		record(ctx, c.Usage, usage.Event{TenantID: tenantID, Kind: usage.KindRerank, Model: model, InputTokens: tokens, Outcome: outcome})
	}()

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.Authorize != nil {
		if err := c.Authorize(hreq); err != nil {
			return nil, err
		}
	}
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		return nil, fmt.Errorf("reranking: cross-encoder status %d", res.StatusCode)
	}
	var resp ceResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&resp); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}

	seen := map[int]bool{}
	type scored struct {
		i     int
		score float64
	}
	var ss []scored
	for _, r := range resp.Records {
		var i int
		if _, err := fmt.Sscan(r.ID, &i); err != nil || i < 0 || i >= len(hits) || seen[i] || fmt.Sprint(i) != r.ID {
			return nil, fmt.Errorf("%w: record id %q", ErrMalformed, r.ID)
		}
		seen[i] = true
		ss = append(ss, scored{i, r.Score})
	}
	if len(ss) == 0 {
		return nil, fmt.Errorf("%w: no records", ErrMalformed)
	}
	sort.SliceStable(ss, func(a, b int) bool { return ss[a].score > ss[b].score })
	for _, s := range ss[:min(keep, len(ss))] {
		h := hits[s.i]
		h.RerankScore = s.score
		out = append(out, h)
	}
	return out, nil
}
