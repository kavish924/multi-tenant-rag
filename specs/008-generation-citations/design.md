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

None (reads `usage_events`). Optional `prompt_snapshot` table (eval/debug) — **default:** log
structured prompt metadata only, no doc text (cardinality + logging rules in `010`).

## 5. API contract

`POST /tenants/{id}/ask` → SSE. Events:

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
