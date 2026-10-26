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
