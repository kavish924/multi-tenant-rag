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
