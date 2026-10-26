# 011 — Quotas & Cost Control: Requirements

- **Spec ID:** 011 `QTA`
- **Status:** Draft
- **Milestone:** 4 — Observable production
- **Depends on:** `specs/008-generation-citations` (usage recording), `010-observability`
- **Shared context:** `docs/system-design.md §9` (excessive LLM spending), `§10` budget layer

## Overview

Cost is a security boundary ("denial of wallet"). Per-tenant quotas (uploads, questions, token
budgets), concurrency caps, and circuit breakers are enforced **before** provider spend; usage
from `usage_events` is aggregated into spend views. Rejections are explicit (429 + Retry-After).

## User stories

- **QTA-US-1:** As a platform operator, I want per-tenant quotas so one tenant cannot run up my LLM bill.
- **QTA-US-2:** As a tenant admin, I want to see my tenant's usage and limits so I can manage consumption.
- **QTA-US-3:** As a platform operator, I want provider outages to trip circuit breakers so failures shed load instead of piling latency.

## Requirements

### Functional

- **QTA-REQ-001:** Quota dimensions (defaults configurable per tenant): `uploads/day` (default 500), `questions/day` (default 1,000), `embed_tokens/month` (default 50M), `generate_tokens/month` (default 20M), `concurrent_asks` (default 5), `concurrent_worker_jobs_per_tenant` (default 3).
- **QTA-REQ-002:** Enforcement points: upload handler (pre-enqueue), ask handler (pre-retrieval — **before** any provider call), worker claim (per-tenant job concurrency).
- **QTA-REQ-003:** Token-budget checks estimate before the call (question embedding size, prompt size × context) and reconcile with actuals from `usage_events` after.
- **QTA-REQ-004:** Rejections: `429` with `Retry-After` and machine-readable `code` (`quota_uploads`, `quota_questions`, `quota_tokens`, `concurrency`).
- **QTA-REQ-005:** Circuit breaker per provider (embed/generate/rerank): open after N consecutive failures or sustained 429s; half-open probes; open state fails fast (ask → 503 `provider_unavailable` with fallback messaging; ingestion → job retry ladder unaffected).
- **QTA-REQ-006:** `GET /tenants/{id}/usage` returns current-period counters and limits per dimension (admin+).
- **QTA-REQ-007:** Cost estimation: per-kind price config (input/output per 1k tokens) → `estimated_cost_usd_total` metric and usage endpoint figure.
- **QTA-REQ-008:** Overrides: platform operator can set per-tenant quota overrides (table, not config redeploy).

### Non-functional

- **QTA-NFR-001:** Quota check adds < 10 ms p95 (counter cache or single indexed query).
- **QTA-NFR-002:** Quota enforcement is race-safe under concurrent requests (no overdraft beyond a documented small tolerance, default 1%).

## Acceptance criteria (EARS)

- **QTA-AC-001:** WHEN a tenant exceeds `questions/day`, THE SYSTEM SHALL respond 429 before any embedding or LLM call for that request.
- **QTA-AC-002:** WHEN a tenant exceeds `uploads/day`, THE SYSTEM SHALL reject the upload with 429 and store nothing.
- **QTA-AC-003:** WHEN projected generate tokens exceed the remaining monthly budget, THE SYSTEM SHALL reject with 429 `quota_tokens` pre-call.
- **QTA-AC-004:** WHEN a tenant already runs `concurrent_asks` streams, THE SYSTEM SHALL queue-limit new asks with 429 `concurrency` until slots free.
- **QTA-AC-005:** WHEN 100 concurrent same-tenant asks race the quota at 1 remaining, THE SYSTEM SHALL admit at most the documented tolerance (QTA-NFR-002) and no more.
- **QTA-AC-006:** WHEN a provider circuit opens, THE SYSTEM SHALL fail fast with 503 and stop sending new calls until half-open probing succeeds.
- **QTA-AC-007:** WHEN quotas are overridden for a tenant, THE SYSTEM SHALL apply them on the next request without redeploy.
- **QTA-AC-008:** WHEN usage is queried, THE SYSTEM SHALL report counters consistent with `usage_events` aggregates for the period.

## Out of scope

- Billing/invoicing; payment integration; per-question pricing tiers.
