# 011 — Quotas & Cost Control: Design

- **Spec ID:** 011 `QTA` · **Milestone:** 4
- **Shared design:** [system-design.md §9/§10](../../docs/system-design.md#9-production-security-and-reliability) (budget layer)

## 1. Context and goals

Enforce "spend limits" deterministically before money burns. The budget guard layer of the
guardrail design, materialized as middleware + worker gate + provider circuit breakers.

## 2. Scope

**In:** quota dimensions/checks, overrides table, usage endpoint, circuit breakers, cost
estimation. **Out:** billing UI; pricing strategy.

## 3. Components

- `internal/quotas`: `Limiter` — fixed-window counters per (tenant, dimension, period) stored in
  Postgres (`tenant_quotas` overrides + `tenant_usage_counters`), with a short-TTL in-process
  cache for check-path speed; reconciliation from actual usage.
- `internal/breaker`: per-provider circuit breaker (failure-count/429-rate trigger, half-open probe).
- Middleware wiring in ask/upload handlers; worker claim gate via per-tenant semaphore in `cmd/worker`.

## 4. Data model changes

```sql
CREATE TABLE tenant_quotas (
  tenant_id uuid PRIMARY KEY REFERENCES tenants(id),  -- NULL-row = defaults
  uploads_day int, questions_day int,
  embed_tokens_month bigint, generate_tokens_month bigint,
  concurrent_asks int, concurrent_jobs int, updated_at timestamptz);

CREATE TABLE tenant_usage_counters (         -- fixed windows, reconciled by usage_events
  tenant_id uuid NOT NULL, dimension text NOT NULL,
  window_start timestamptz NOT NULL, used bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (tenant_id, dimension, window_start));
```

## 5. API contract

- Ask/upload 429 envelope: `{"error":{"code":"quota_questions","retry_after":3600,"limit":1000,"used":1000}}`.
- `GET /tenants/{id}/usage` → `{period, dimensions:{questions:{limit,used}, ...}, estimated_cost_usd}`.

## 6. Key flows

**Ask gate:** resolve tenant → load effective quotas (defaults ⊕ overrides, cached) → check
`questions/day` + projected `generate_tokens` + concurrency slot → any failure ⇒ 429/503 **before**
retrieval (QTA-AC-001/003). On stream end, actuals reconcile counters (small drift acceptable,
QTA-NFR-002 tolerance).

**Worker gate:** claim loop consults per-tenant active-job gauge; skip tenant at cap (fair poll).

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Counter row contention | UPSERT conflict | retry once, else conservative deny |
| Cache staleness | TTL 5s | tolerance documented |
| Provider 429/failure storm | breaker counters | open → fail fast (QTA-AC-006) |

## 8. Security and multi-tenancy

Quota checks run inside tenant context; overrides readable only for the authenticated tenant
(operator override path is an internal/admin tool). Prevents both accidental and malicious
spend (risk table: "Excessive LLM spending").

## 9. Testing strategy

- Unit: window math; projection estimation; breaker state machine.
- Integration: quota exhaustion per dimension (QTA-AC-001/002/003); override hot-apply (AC-007);
  usage endpoint consistency (AC-008).
- Concurrency/load: 100-race admission tolerance (QTA-AC-005); breaker open/half-open transitions (AC-006).

## 10. Open questions

- Sliding windows vs fixed → **default:** fixed windows (simple, race-safe); revisit with data.

## 11. As built

- `migrations/0010_quotas.sql`: `tenant_quotas` (overrides; NULL = default; `app` SELECT only),
  `tenant_usage_counters` (UTC day windows for `uploads`/`questions`, UTC month windows for
  `embed_tokens`/`generate_tokens`), `ask_slots` (one row per live stream, expiring after 16 min),
  all under forced RLS. `claim_job` gains `p_tenant_cap` (system function list updated).
- `quotas.Limiter`: effective limits = defaults ⊕ overrides, cached 5 s (an override applies
  within 5 s, QTA-AC-007). **Counts** (`uploads`, `questions`) use a conditional UPSERT
  (`... DO UPDATE ... WHERE used + n <= limit`), which serializes on the counter row: zero
  overdraft (QTA-AC-005). **Token budgets** are checked against the projection before the call
  and charged with actuals by `usage.Recorder` in the same transaction as the `usage_events` row,
  so the counters always equal the aggregates (QTA-AC-008); concurrent calls may overshoot by at
  most `concurrent_asks × projection` (~5 × 7k = 35k of 20M = 0.2%, inside the 1% tolerance).
  `rerank` usage draws from `generate_tokens`; ingestion now records `embed` usage (chunk tokens).
- **Concurrency:** `AcquireAskSlot` takes a per-tenant transaction advisory lock, drops expired
  slots, counts and inserts — exact across API instances. The worker gate is in `claim_job`:
  a tenant at its running-job cap is skipped; two claimers racing can exceed it by at most
  (workers − 1) for one claim.
- **Ask gate order** (before retrieval): generator circuit open → 503; ask slot → embed and
  generate token checks → reserve one question. A failure before the SSE stream opens refunds
  the question; the slot is released when the request ends. Projection = question + 6,000
  context + 1,024 output tokens (+ the listwise rerank prompt when reranking is on).
- **Upload gate:** one upload is reserved before the body is read and refunded unless a new
  document was created (validation errors and duplicates cost nothing).
- **Breakers** (`internal/breaker`): `Threshold` consecutive failures (errors and 429s; caller
  cancellations ignored) open the circuit for `OpenFor`; then one probe at a time; a failed
  probe re-opens. `breaker.Embedder` / `breaker.Generator` wrap providers (one breaker per
  provider per process). Ask: open → 503 `provider_unavailable` with `Retry-After: 30`;
  ingestion: `ErrOpen` is a retryable embed failure, so the retry ladder is unchanged.
- **API:** 429 envelope per §5 with `Retry-After` (to the window end; 5 s for concurrency).
  `GET /tenants/{tenant}/usage` (admin+): per-dimension `{limit, used, window_start,
  window_end}`, live asks, job cap, and `estimated_cost_usd` for the month from `usage_events`
  × `observability.Prices`.
- **Operator path:** `cmd/quotactl show|set|reset -tenant <id>`, run with the migrator's
  credentials (the app role cannot write overrides). `set` replaces the whole override row.
- Config: `QUOTA_*` defaults, `BREAKER_THRESHOLD` (5), `BREAKER_OPEN_FOR` (30s).
- Not yet: an embed-budget gate inside ingestion (ingestion usage is counted but not blocked),
  and cleanup of old counter windows.
