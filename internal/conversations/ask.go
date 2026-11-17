package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/kavish924/multi-tenant-rag/internal/auth"
	"github.com/kavish924/multi-tenant-rag/internal/breaker"
	"github.com/kavish924/multi-tenant-rag/internal/guard"
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
	// Guards is the guardrail policy (spec 012); nil uses guard.DefaultPolicy.
	Guards     *guard.Policy
	policyOnce sync.Once
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

// Ask implements the endpoint with the guard layers of spec 012:
// probing throttle → input stages (400) → budget: circuit breakers (503)
// and quotas (429) → retrieve → rerank → retrieval hygiene (refusal) →
// prompt → stream through the output filter (redaction, leak detection;
// windowed or strict) → final stages (faithfulness) → citations → done.
func (h *Handler) Ask(w http.ResponseWriter, r *http.Request) {
	log := h.log()
	pol := h.policy()
	m, ok := tenants.FromContext(r.Context())
	if !ok {
		httpx.Forbidden(w)
		return
	}
	userID, _ := auth.UserFrom(r.Context())
	var req askRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	ctx := r.Context() // cancelled when the client disconnects (GEN-REQ-004)

	// GRD-REQ-002: repeated guard blocks from one user escalate to 429.
	if throttled, left := pol.Probing.Throttled(m.TenantID, userID); throttled {
		quotas.WriteExceeded(w, &quotas.Exceeded{Code: "abuse_suspected", RetryAfter: left})
		return
	}
	// GRD-AC-003: input stages before retrieval.
	if d := guard.Evaluate(ctx, req.Question, pol.InputStages()...); d.Blocked() {
		pol.Observe(ctx, m.TenantID, userID, d)
		msg := "question must be 1 to " + strconv.Itoa(retrieval.MaxQuestionRunes) + " characters of plain text"
		httpx.Error(w, http.StatusBadRequest, "invalid_question", msg)
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

	// GRD-REQ-009 / QTA-AC-006: an open provider circuit is a budget-stage
	// Block and fails fast with 503, before any spend.
	if a, ok := h.Generator.(guard.Availability); ok {
		if d := guard.Evaluate(ctx, question, guard.ProviderAvailable{Providers: []guard.Availability{a}}); d.Blocked() {
			pol.Observe(ctx, m.TenantID, userID, d)
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
	refuse := func(d guard.Decision, released bool) {
		pol.Observe(ctx, m.TenantID, userID, d)
		send("refusal", refusalPayload{
			Code: "guard_blocked", Stage: d.Stage, Discard: released,
			Message: "The answer was withheld by a safety check.",
		})
	}

	// GRD-AC-004: tenant purity, deleted exclusion and the document cap are
	// asserted, not assumed. A violation is a bug: refuse and alert.
	if err := guard.AssertHygiene(m.TenantID, hits, pol.DocCap()); err != nil {
		refuse(guard.Decision{Verdict: guard.Block, Stage: guard.StageRetrievalHygiene, Reason: "blocked", Err: err}, false)
		return
	}

	// GEN-AC-005 / GEN-REQ-009: no sources, no model call, no fabrication.
	if len(hits) == 0 {
		log.InfoContext(ctx, "ask: no sources; insufficient context", "tenant_id", m.TenantID)
		if send("token", tokenPayload{T: llm.InsufficientContext}) {
			send("done", donePayload{Citations: []Citation{}, InsufficientContext: true})
		}
		return
	}

	// GRD-AC-005: highest-ranked chunks within the context budget, delimited
	// as untrusted data.
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

	out := pol.NewOutput(leakDetector)
	gotToken := false
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
			// The unreleased tail is discarded: it never passed the final checks.
			log.ErrorContext(ctx, "ask: generation failed mid-stream", "tenant_id", m.TenantID, "err", err)
			send("error", errorPayload{Message: "generation failed"})
			return
		}
		if !gotToken {
			gotToken = true
			first.End()
		}
		emit, d := out.Write(ctx, tok)
		if d.Blocked() {
			cancel() // GRD-AC-002: stop the provider; later stages never run
			gen.Outcome = usage.OutcomeCancelled
			gen.InputTokens, gen.OutputTokens = stream.Usage().InputTokens, stream.Usage().OutputTokens
			refuse(d, out.Released())
			return
		}
		if emit != "" && !send("token", tokenPayload{T: emit}) {
			cancel() // write failed: the client is gone, stop spending tokens
			gen.Outcome = usage.OutcomeCancelled
			gen.InputTokens, gen.OutputTokens = stream.Usage().InputTokens, stream.Usage().OutputTokens
			return
		}
	}
	u := stream.Usage()
	gen.InputTokens, gen.OutputTokens = u.InputTokens, u.OutputTokens

	// Final stages over the whole cleaned answer (GRD-REQ-008, optional).
	final := pol.FinalStages(p.Sources, func(label string, err error) {
		if err != nil {
			log.WarnContext(ctx, "ask: faithfulness check failed", "tenant_id", m.TenantID, "err", err)
			return
		}
		log.InfoContext(ctx, "ask: faithfulness", "tenant_id", m.TenantID, "label", label)
	})
	rest, d := out.Finish(ctx, final...)
	if d.Blocked() {
		refuse(d, out.Released())
		return
	}
	if out.Redacted() {
		pol.Observe(ctx, m.TenantID, userID, guard.Decision{Verdict: guard.Redact, Stage: guard.StageOutputRedact, Reason: "redacted"})
	}
	if rest != "" && !send("token", tokenPayload{T: rest}) {
		return
	}

	// GRD-AC-007: only citations that resolve to this request's sources.
	answer := out.Text()
	citations := ResolveCitations(answer, p.Sources)
	if len(citations) == 0 && !strings.Contains(answer, llm.InsufficientContext) {
		// GRD-REQ-006: a soft signal, not a Block.
		observability.M().UncitedAnswer(ctx, m.TenantID)
		log.InfoContext(ctx, "ask: answer cites no source", "tenant_id", m.TenantID)
	}
	for _, c := range citations {
		if !send("citation", c) {
			return
		}
	}
	send("done", donePayload{Citations: citations, Usage: u})
}

// refusalPayload replaces a blocked answer. Discard tells the client that
// text it already rendered (windowed mode) must be removed.
type refusalPayload struct {
	Code    string `json:"code"`
	Stage   string `json:"stage"`
	Message string `json:"message"`
	Discard bool   `json:"discard"`
}

// leakDetector indexes the system prompt once.
var leakDetector = guard.NewLeakDetector(SystemPrompt)

func (h *Handler) policy() *guard.Policy {
	h.policyOnce.Do(func() {
		if h.Guards == nil {
			h.Guards = guard.DefaultPolicy()
		}
		if h.Guards.Log == nil {
			h.Guards.Log = h.Log
		}
	})
	return h.Guards
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
