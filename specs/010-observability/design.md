# 010 — Observability: Design

- **Spec ID:** 010 `OBS` · **Milestone:** 4
- **Shared design:** [system-design.md §7](../../docs/system-design.md#7-observability)

## 1. Context and goals

"Explain any request from telemetry alone." Traces answer *where did time go / why did it
fail*; metrics answer *how is the system trending*; logs correlate the two.

## 2. Scope

**In:** OTel wiring, span plan, metric definitions, structured logging stdlib wrapper, cardinality
enforcement, compose services (collector, Prometheus, Grafana + dashboards).
**Out:** alerting rules; retention policy.

## 3. Components

- `internal/observability`: `Setup(ctx, service)` (tracer/meter providers, OTLP exporter),
  `Logger(ctx)` (slog JSON with correlation fields), span helpers per stage.
- Trace context in jobs: inject `traceparent` into job payload at `Enqueue`; worker extracts
  as parent (links if spans get too deep).
- `deployments/`: otel-collector config (OTLP gRPC), prometheus scrape config, grafana
  provisioning + `dashboards/rag.json`.

## 4. Data model changes

None. (Job payload gains a `traceparent` field — backward compatible, ignored if absent.)

## 5. API contract (internal)

Middleware: request id + trace start → `w.Header().Get("X-Request-Id")` echo. Span attribute
conventions: `tenant.id`, `document.id`, `job.id`, `job.kind`, `provider.name` — attributes on
spans/logs only (allowed), never metric labels.

## 6. Key flows

**Ask trace:** `POST /ask` → `retrieve` (embed → vector → fts → fusion → [rerank]) →
`llm.first_token` → `llm.total`. **Ingestion trace:** upload request span → serialized into
job → worker `claim → extract → chunk → embed batch(es) → index` as children (OBS-AC-002).

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Collector unreachable | exporter timeout | drop batch, continue (OBS-AC-006) |
| Metric label abuse (dev mistake) | CI cardinality lint + runtime allowlist | reject/scrub + log |

## 8. Security and multi-tenancy

Logs/metrics never carry document text or secrets (OBS-REQ-006; matches system-design rule).
Tenant id is a bounded label — acceptable per cardinality note (hundreds of tenants).

## 9. Testing strategy

- Unit: log field allowlist; span helper nesting.
- Integration: in-memory span recorder asserts OBS-REQ-002/003 span sets per request/job (OBS-AC-001/002).
- Security/CI: automated scan of metric output for forbidden labels (OBS-AC-003); log-scan for
  doc-text leakage with canary content (OBS-AC-004, ties into `015`).
- Perf: overhead check (OBS-NFR-001).

## 10. Open questions

- Trace sampling strategy (head vs tail) → **default:** head sampling 100% in dev, 10% prod,
  always-keep error traces via tail proxy later.
