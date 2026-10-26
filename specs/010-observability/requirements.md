# 010 — Observability: Requirements

- **Spec ID:** 010 `OBS`
- **Status:** Draft
- **Milestone:** 4 — Observable production
- **Depends on:** all Milestone 1–3 specs
- **Shared context:** `docs/system-design.md §7` (observability)

## Overview

The goal: explain the latency and failure of any request from its telemetry alone. OpenTelemetry
traces across API and worker (trace context travels through the job payload), Prometheus
metrics, structured logs. Cardinality discipline: tenant ids are fine; document/user ids live
in traces and logs, never metric labels.

## User stories

- **OBS-US-1:** As a platform operator, I want one trace per ask showing where time went (embed, search, fusion, rerank, LLM first-token/total).
- **OBS-US-2:** As a platform operator, I want ingestion traces linked to the originating upload request.
- **OBS-US-3:** As a platform operator, I want queue depth/age, failure rates, provider errors, and per-tenant token cost on dashboards.
- **OBS-US-4:** As a developer, I can debug a failed job from its `last_error` + trace without guessing.

## Requirements

### Functional

- **OBS-REQ-001:** OTel tracing in `cmd/api` and `cmd/worker` (OTLP exporter → collector); W3C traceparent accepted on inbound requests.
- **OBS-REQ-002:** Query trace spans: `embed question`, `vector search`, `full-text search`, `fusion`, `rerank`, `llm first-token`, `llm total` — with tenant attribute.
- **OBS-REQ-003:** Ingestion trace spans: `claim`, `extract`, `chunk`, `embed batch`, `index write`; child of the upload request's trace via trace context serialized into the job payload.
- **OBS-REQ-004:** Metrics: `http_request_duration_seconds` (histogram, route label), `jobs_queue_depth`, `jobs_queue_age_seconds`, `jobs_attempts_total`/`jobs_failed_total` (kind), `provider_errors_total` (provider, kind), `tokens_total{tenant_id,kind,direction}`, `estimated_cost_usd_total{tenant_id}`.
- **OBS-REQ-005:** Metric label cardinality rule: allowed labels `route`, `kind`, `status`, `provider`, `tenant_id` (bounded); **forbidden:** document id, user id, chunk id, question text.
- **OBS-REQ-006:** Structured logs (JSON): request id, tenant id, document id, job id, route, duration, error; **never** document text, prompts, answers, or secrets.
- **OBS-REQ-007:** Exemplars link metrics → traces (histogram exemplars or trace id in logs).

### Non-functional

- **OBS-NFR-001:** Telemetry adds < 5% overhead p95.
- **OBS-NFR-002:** Dashboards exist for: ask latency (p50/p95/p99), queue depth/age, job failure rate, provider error rate, tokens & cost per tenant, parser failure reasons.

## Acceptance criteria (EARS)

- **OBS-AC-001:** WHEN an ask completes, THE SYSTEM SHALL have produced a single trace containing all OBS-REQ-002 spans with correct nesting and durations.
- **OBS-AC-002:** WHEN a worker processes a job, THE SYSTEM SHALL produce spans that are children of the upload request trace (linked via job payload context).
- **OBS-AC-003:** WHEN any metric is scraped, THE SYSTEM SHALL expose no document/user/chunk identifiers as labels (automated cardinality test).
- **OBS-AC-004:** WHEN a log line is emitted for a request or job, THE SYSTEM SHALL include the correlation ids and SHALL NOT contain document text or secrets (automated log-scan test).
- **OBS-AC-005:** WHEN a job fails, THE SYSTEM SHALL increment `jobs_failed_total{kind}` and the dashboard panel reflects it within one scrape interval.
- **OBS-AC-006:** WHEN the collector is down, THE SYSTEM SHALL degrade gracefully (drop telemetry, never block requests beyond a small timeout).

## Out of scope

- Alert rules/on-call rotation (ops runbook, later); log aggregation backend choice (collector output is pluggable).
