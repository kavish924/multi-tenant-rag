# 008 — Generation, Streaming & Citations: Design

- **Spec ID:** 008 `GEN` · **Milestone:** 3
- **Shared design:** [system-design.md §6](../../docs/system-design.md#6-generation-streaming-and-citations)

## 1. Context and goals

Turn retrieval hits into a streamed, attributable answer. Two invariants: the model answers
only from provided context, and citations resolve only to this request's retrieved records.

## 2. Scope

**In:** SSE handler, prompt builder, citation resolver, usage recording, disconnect handling.
**Out:** guards (`012`), quotas (`011`), rerank (`009`).

## 3. Components

- `internal/conversations`: `Handler.Ask` (SSE), `buildRequest`, `ResolveCitations`,
  `writeSSE`, usage recorder.
- `internal/llm`: `Generator` stream interface (single stream abstraction per tech-stack note:
  token events + terminal error + final usage).

## 4. Data model changes

`usage_events` (`migrations/0008_usage_events.sql`): `tenant_id, kind (embed|generate|rerank),
model, input_tokens, output_tokens, outcome (ok|error|cancelled), request_id, created_at`, RLS;
`app` may `SELECT`/`INSERT` only. Optional `prompt_snapshot` table (eval/debug) — **default:** log
structured prompt metadata only, no doc text (cardinality + logging rules in `010`).

## 5. API contract

`POST /tenants/{tenant}/ask` (and `POST /ask`, tenant from `X-Tenant-ID` or the sole
membership, like `/documents`) with `{"question": "..."}` → SSE. Events:

```text
event: token     data: {"t":"..."}
event: citation  data: {"n":2,"chunk_id":"...","document_id":"...","page":4,"heading":"..."}
event: done      data: {"citations":[...],"usage":{"input":..., "output":...}}
event: error     data: {"message":"generation failed"}
```

## 6. Key flows

**Ask:** authn/tenant (001) → quota precheck (011) → retrieve (007) → hits empty ⇒
insufficient-context path → prompt build (numbered, delimited) → `gen.Stream(ctx, req)` →
flush tokens → on completion resolve citations → emit citation(s) + done → usage event.
Context from `r.Context()` flows into provider (GEN-AC-004).

**Citation resolution:** regex `\[(\d+)\]` over final answer text → filter `1 ≤ n ≤ len(hits)`,
dedupe → map to `hits[n-1]` (the shared `ResolveCitations` code).

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Retrieval error | Search error | 502, stream never opened (GEN-AC-006) |
| Provider error mid-stream | stream error event | one `error` SSE event, close, usage recorded (GEN-AC-007) |
| Client disconnect | ctx cancel | provider ctx cancelled (GEN-AC-004) |
| Flusher unsupported | type assertion | 500 "streaming unsupported" |

## 8. Security and multi-tenancy

Hits are already tenant-pure from `007`; citations can only reference those hits (GEN-AC-003).
Dereferencing re-checks authz (GEN-AC-009). Document text is delimited untrusted data — guard
layers in `012` wrap this endpoint.

## 9. Testing strategy

- Unit: prompt builder snapshot; `ResolveCitations` (valid/out-of-range/duplicate/non-numeric).
- Integration: httptest SSE client — event order, framing, flush timing; usage row written;
  zero-hit path; retrieval-failure 502.
- Failure: provider error mid-stream; disconnect cancellation timing (GEN-AC-004/NFR-002).
- Security (with `015`): injected `[3]` referencing another tenant's chunk id is unrepresentable
  (ids come from hits only).

## 10. Open questions

- Stream partial citations as they appear vs at end → **default:** resolve at end (model may
  cite any chunk until done); revisit for UX.

## 11. As built

- `internal/llm`: `Generator.Stream(ctx, GenerateRequest) (Stream, error)`; `Stream` is
  `Recv() (string, error)` (io.EOF at the end, ctx error on cancel), `Usage()`, `Close()`.
  `FakeGenerator` answers extractively from source 1 with `[1]` (deterministic dev/test
  provider; options for scripted answers, per-token delay and injected mid-stream failure).
  The Vertex AI Gemini provider lands with the GCP work.
- `internal/conversations`: `BuildPrompt` (sources `<source n="i" document=… page=… heading=…>`
  inside `<sources>`, all document text and the question HTML-escaped so nothing can close a
  delimiter; context budget 6,000 approx. tokens, first hit always kept), `ResolveCitations`
  (`\[(\d{1,4})\]`, range check, dedupe, first-appearance order), `Handler.Ask`.
- Top 8 hits go into the prompt (RET-REQ-004). Zero hits ⇒ a fixed insufficient-context answer
  **without calling the model** (cheaper and cannot fabricate), `done.insufficient_context = true`.
- `done` carries `citations`, `usage {input, output}` and `insufficient_context`. Citation
  events carry `n, chunk_id, document_id, filename, page, heading`.
- Usage: one `embed` row per successful retrieval (question tokens, approximate) and one
  `generate` row on every path once the model was called, with `outcome` ok/error/cancelled.
- A failed SSE write (client gone) cancels the provider context at once; otherwise the request
  context does (GEN-AC-004). Prompt metadata (source count, chunk ids, context tokens, model)
  is logged; document text never is (GEN-AC-008).
- `statusRecorder` (request logger) now implements `http.Flusher` and `Unwrap`.
