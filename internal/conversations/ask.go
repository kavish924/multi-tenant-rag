package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/kavish924/multi-tenant-rag/internal/breaker"
	"github.com/kavish924/multi-tenant-rag/internal/httpx"
	"github.com/kavish924/multi-tenant-rag/internal/llm"
	"github.com/kavish924/multi-tenant-rag/internal/observability"
	"github.com/kavish924/multi-tenant-rag/internal/quotas"
	"github.com/kavish924/multi-tenant-rag/internal/reranking"
	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
	"github.com/kavish924/multi-tenant-rag/internal/tenants"
	"github.com/kavish924/multi-tenant-rag/internal/usage"
)

// DefaultTopK is how many hits go into the prompt (RET-REQ-004: 8 out).
const DefaultTopK = 8

// DefaultMaxOutputTokens bounds one answer.
const DefaultMaxOutputTokens = 1024

// Handler serves POST /ask (spec 008). It runs behind Authenticate →
// ResolveTenant → Authorize(member); the tenant comes only from the
// verified membership.
type Handler struct {
	Retriever retrieval.Retriever
	Generator llm.Generator
	Usage     *usage.Recorder // nil disables usage recording (unit tests)
	// Rerank is the optional stage between fusion and prompt (009); nil or
	// disabled passes the fused top-K straight through.
	Rerank *reranking.Stage
	// Quotas is the pre-provider gate (spec 011); nil disables quotas.
	Quotas *quotas.Limiter
	// EmbedModel names the question embedder for the embed usage row.
	EmbedModel       string
	TopK             int
	MaxContextTokens int
	MaxOutputTokens  int
	Log              *slog.Logger
}

type askRequest struct {
	Question string `json:"question"`
}

// donePayload is the terminal event.
type donePayload struct {
	Citations           []Citation `json:"citations"`
	Usage               llm.Usage  `json:"usage"`
	InsufficientContext bool       `json:"insufficient_context,omitempty"`
}

// Ask implements the endpoint: validate → retrieve → (zero hits: fixed
// insufficient-context answer) → prompt → stream tokens → resolve
// citations → citation events → done → usage.
func (h *Handler) Ask(w http.ResponseWriter, r *http.Request) {
	log := h.log()
	m, ok := tenants.FromContext(r.Context())
	if !ok {
		httpx.Forbidden(w)
		return
	}
	var req askRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	question, err := retrieval.ValidateQuestion(req.Question)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_question", err.Error())
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		httpx.Error(w, http.StatusInternalServerError, "streaming_unsupported", "streaming unsupported")
		return
	}
	ctx := r.Context() // cancelled when the client disconnects (GEN-REQ-004)

	// QTA-AC-006: an open provider circuit fails fast, before any spend.
	if a, ok := h.Generator.(interface{ Available() error }); ok {
		if err := a.Available(); err != nil {
			providerUnavailable(w)
			return
		}
	}
	// QTA-REQ-002: the quota gate runs before retrieval, so a rejected ask
	// never reaches the embedding or generation provider (QTA-AC-001/003/004).
	streamed := false
	if h.Quotas != nil {
		adm, err := h.Quotas.AdmitAsk(ctx, m.TenantID, int64(llm.ApproxTokens(question)), h.projectedGenerateTokens(question))
		var ex *quotas.Exceeded
		if errors.As(err, &ex) {
			log.InfoContext(ctx, "ask: quota rejection", "tenant_id", m.TenantID, "code", ex.Code)
			quotas.WriteExceeded(w, ex)
			return
		}
		if err != nil {
			log.ErrorContext(ctx, "ask: quota check failed", "tenant_id", m.TenantID, "err", err)
			httpx.Internal(w)
			return
		}
		defer func() {
			if streamed {
				adm.Done(ctx)
			} else {
				adm.Abort(ctx) // failed before the stream opened: the question is refunded
			}
		}()
	}

	hits, err := h.Retriever.Search(ctx, m.TenantID, question, h.Rerank.Want(h.topK()))
	if err != nil {
		if errors.Is(err, retrieval.ErrEmptyQuestion) || errors.Is(err, retrieval.ErrQuestionTooLong) {
			httpx.Error(w, http.StatusBadRequest, "invalid_question", err.Error())
			return
		}
		if ctx.Err() != nil {
			return // client gone; nothing to answer
		}
		if errors.Is(err, breaker.ErrOpen) {
			providerUnavailable(w)
			return
		}
		log.ErrorContext(ctx, "ask: retrieval failed", "tenant_id", m.TenantID, "err", err)
		// GEN-AC-006: 502 before the stream opens.
		httpx.Error(w, http.StatusBadGateway, "retrieval_failed", "retrieval failed")
		return
	}
	h.record(ctx, usage.Event{
		TenantID: m.TenantID, Kind: usage.KindEmbed, Model: h.EmbedModel,
		InputTokens: llm.ApproxTokens(question),
	})
	if h.Rerank.Active() {
		// Fails open: any reranker problem keeps the fused order (RER-REQ-003).
		hits = h.Rerank.Apply(ctx, m.TenantID, question, hits)
	} else if len(hits) > h.topK() {
		hits = hits[:h.topK()]
	}

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	streamed = true
	fl.Flush()
	send := func(event string, v any) bool {
		if err := writeSSE(w, event, v); err != nil {
			return false
		}
		fl.Flush()
		return true
	}

	// GEN-AC-005 / GEN-REQ-009: no sources, no model call, no fabrication.
	if len(hits) == 0 {
		log.InfoContext(ctx, "ask: no sources; insufficient context", "tenant_id", m.TenantID)
		if send("token", tokenPayload{T: llm.InsufficientContext}) {
			send("done", donePayload{Citations: []Citation{}, InsufficientContext: true})
		}
		return
	}

	p := BuildPrompt(question, hits, h.MaxContextTokens)
	p.Request.MaxOutputTokens = h.maxOutput()
	// GEN-AC-008: prompt metadata only, never document text.
	log.InfoContext(ctx, "ask: prompt", "tenant_id", m.TenantID, "sources", len(p.Sources),
		"chunk_ids", chunkIDs(p.Sources), "context_tokens", p.ContextTokens, "model", h.Generator.Model())

	genCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	gen := usage.Event{TenantID: m.TenantID, Kind: usage.KindGenerate, Model: h.Generator.Model()}
	defer func() { h.record(ctx, gen) }() // every terminal path (GEN-AC-007)

	// OBS-REQ-002: "llm total" spans the whole stream, "llm first-token"
	// the wait for the first fragment.
	genCtx, total := observability.Start(genCtx, "llm total", observability.AttrProvider.String(h.Generator.Model()))
	_, first := observability.Start(genCtx, "llm first-token")
	var genErr error
	defer func() {
		first.End() // no-op if already ended
		if genErr != nil && ctx.Err() == nil {
			observability.M().ProviderError(ctx, h.Generator.Model(), "generate")
		}
		observability.End(total, genErr)
	}()
	stream, err := h.Generator.Stream(genCtx, p.Request)
	if err != nil {
		genErr = err
		gen.Outcome = outcome(ctx)
		log.ErrorContext(ctx, "ask: generation did not start", "tenant_id", m.TenantID, "err", err)
		send("error", errorPayload{Message: "generation failed"})
		return
	}
	defer func() { _ = stream.Close() }()

	var answer strings.Builder
	for {
		tok, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			genErr = err
			gen.Outcome = outcome(ctx)
			gen.InputTokens, gen.OutputTokens = stream.Usage().InputTokens, stream.Usage().OutputTokens
			if ctx.Err() != nil {
				log.InfoContext(ctx, "ask: client disconnected; generation cancelled", "tenant_id", m.TenantID)
				return
			}
			log.ErrorContext(ctx, "ask: generation failed mid-stream", "tenant_id", m.TenantID, "err", err)
			send("error", errorPayload{Message: "generation failed"})
			return
		}
		if answer.Len() == 0 {
			first.End()
		}
		answer.WriteString(tok)
		if !send("token", tokenPayload{T: tok}) {
			cancel() // write failed: the client is gone, stop spending tokens
			gen.Outcome = usage.OutcomeCancelled
			gen.InputTokens, gen.OutputTokens = stream.Usage().InputTokens, stream.Usage().OutputTokens
			return
		}
	}
	u := stream.Usage()
	gen.InputTokens, gen.OutputTokens = u.InputTokens, u.OutputTokens

	citations := ResolveCitations(answer.String(), p.Sources)
	for _, c := range citations {
		if !send("citation", c) {
			return
		}
	}
	send("done", donePayload{Citations: citations, Usage: u})
}

type tokenPayload struct {
	T string `json:"t"`
}

type errorPayload struct {
	Message string `json:"message"`
}

// writeSSE writes one event. data is JSON, which never contains a raw
// newline, so one data: line suffices; the blank line dispatches the event.
func writeSSE(w io.Writer, event string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	return err
}

func (h *Handler) record(ctx context.Context, e usage.Event) {
	if h.Usage == nil || e.Model == "" {
		return
	}
	if err := h.Usage.Record(ctx, e); err != nil {
		h.log().Error("ask: record usage", "tenant_id", e.TenantID, "kind", e.Kind, "err", err)
	}
}

func outcome(ctx context.Context) string {
	if ctx.Err() != nil {
		return usage.OutcomeCancelled
	}
	return usage.OutcomeError
}

func chunkIDs(hits []retrieval.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ChunkID.String()
	}
	return out
}

func (h *Handler) topK() int {
	if h.TopK > 0 {
		return h.TopK
	}
	return h.Rerank.KeepOr(DefaultTopK)
}

func (h *Handler) maxOutput() int {
	if h.MaxOutputTokens <= 0 {
		return DefaultMaxOutputTokens
	}
	return h.MaxOutputTokens
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

// providerUnavailable is the fail-fast response while a circuit is open.
func providerUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "30")
	httpx.Error(w, http.StatusServiceUnavailable, "provider_unavailable",
		"the AI provider is temporarily unavailable; please retry shortly")
}

// projectedGenerateTokens estimates one ask's generation spend before the
// call (QTA-REQ-003): the question, the full context budget, the output
// cap, and the listwise rerank prompt when reranking is on.
func (h *Handler) projectedGenerateTokens(question string) int64 {
	ctxTokens := h.MaxContextTokens
	if ctxTokens <= 0 {
		ctxTokens = DefaultMaxContextTokens
	}
	n := int64(llm.ApproxTokens(question) + ctxTokens + h.maxOutput())
	if h.Rerank.Active() {
		n += int64(h.Rerank.Want(h.topK()) * reranking.MaxPassageBytes / 4)
	}
	return n
}
