# 009 — Reranking: Design

- **Spec ID:** 009 `RER` · **Milestone:** 3
- **Shared design:** [system-design.md §5](../../docs/system-design.md#5-hybrid-retrieval) — "measure whether reranking earns its latency and cost before keeping it"

## 1. Context and goals

Optional precision boost with strict degradation semantics. The interesting engineering is the
fail-open path and measurement, not the model.

## 2. Scope

**In:** `Reranker` interface, cross-encoder + LLM implementations, config, fallback + usage
instrumentation. **Out:** provider hosting; eval math (`014`).

## 3. Components

`internal/reranking`:

```go
type Reranker interface {
    Rerank(ctx context.Context, tenantID uuid.UUID, question string,
           hits []retrieval.Hit, keep int) ([]retrieval.Hit, error)
}
```

Implementations: `crossEncoderReranker` (hosted scoring API), `llmReranker` (listwise prompt
returns ordered ids). Wired in the ask path between `Search` and prompt build; guarded by config.

## 4. Data model changes

None (writes `usage_events` via `008`'s recorder, `kind='rerank'`).

## 5. API contract (internal)

See interface above. Config: `RERANK_ENABLED`, `RERANK_PROVIDER`, `RERANK_KEEP`, `RERANK_TIMEOUT`.

## 6. Key flows

**Enabled path:** fused top-50 → `Rerank(..., keep=8)` with timeout wrapper → success ⇒ reranked
order; failure/timeout ⇒ fallback = first 8 fused hits + `rerank_fallback` increment.
**Disabled path:** first 8 fused hits directly (RER-AC-001).

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Timeout (800 ms) | ctx deadline | fallback (RER-AC-002) |
| Provider 4xx/5xx | error | fallback + metric |
| Malformed LLM ordering output | parse guard | fallback |

Never propagate rerank failure to the user — degrade, don't fail.

## 8. Security and multi-tenancy

Provider requests carry only this tenant's hit texts for this request (RER-REQ-006); the
`tenantID` parameter exists for tenant-bound credentials/quotas in providers.

## 9. Testing strategy

- Unit: fallback ordering identity; keep-truncation; identity preservation.
- Integration: disabled path (no provider calls — mock asserts); timeout injection → fallback
  (RER-AC-001/002/003).
- Measurement: `014` harness A/B artifact (RER-AC-005, RER-NFR-002).

## 10. Open questions

- Cache rerank scores keyed by (chunk_id, question-hash) → **default:** no cache initially
  (correctness first; cache risks cross-request staleness — and any cache key MUST include
  tenant, per red-team "cache leakage" category).

## 11. As built

- `reranking.Stage{Enabled, Reranker, Keep, Timeout, Candidates, OnFallback}` wraps any
  `Reranker`. A nil or disabled stage never calls a provider; the ask handler then asks retrieval
  for 8 hits directly. Enabled, it asks retrieval for 50, reranks under an 800 ms timeout and keeps 8.
- The stage **re-maps the reranker's output to the original candidates by chunk id** and
  rejects unknown or duplicate ids, so an implementation cannot alter content, document ids or
  citation targets (RER-REQ-005). Rank is renumbered; `Hit.RerankScore` carries the score.
- Fallback reasons: `timeout`, `error`, `invalid` (malformed or non-candidate output), logged
  at warn and passed to `OnFallback`, the `rerank_fallback` metric hook for `010`.
- `reranking.LLM`: listwise prompt (passages escaped and capped at 600 bytes each), output must
  match `3, 1, 7` exactly (in range, no duplicates), otherwise `ErrMalformed`.
- `reranking.CrossEncoder`: Vertex AI Ranking API shape (`records [{id, content}]` → `records
  [{id, score}]`, ids are candidate positions, never database ids). `Authorize` hook for the
  OAuth token (GCP work).
- Usage: every call writes `usage_events(kind='rerank')` with its outcome. Both implementations
  record **approximate input tokens** (the cross-encoder: query + records) rather than item
  counts, so quotas (`011`) sum one unit; the model column tells providers apart.
- Config (`cmd/api`): `RERANK_ENABLED` (default false), `RERANK_PROVIDER` (`llm` |
  `cross_encoder`), `RERANK_KEEP` (8), `RERANK_TIMEOUT` (800ms), `RERANK_ENDPOINT`,
  `RERANK_MODEL`. With the dev fake generator the LLM reranker's output is malformed, so it
  always falls back, which exercises the fail-open path.
