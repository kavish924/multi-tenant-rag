# 010 — Observability: Design

- **Spec ID:** 010 `OBS` · **Milestone:** 4
- **Shared design:** [system-design.md §7](../../docs/system-design.md#7-observability)

## 1. Context and goals

"Explain any request from telemetry alone." Traces answer *where did time go / why did it
fail*; metrics answer *how is the system trending*; logs correlate the two.

## 2. Scope

**In:** OTel wiring, span plan, metric definitions, structured logging stdlib wrapper, cardinality
enforcement, OTel collector sidecar config, Cloud Monitoring dashboards (Terraform).
**Out:** alerting rules; retention policy.

## 3. Components

- `internal/observability`: `Setup(ctx, service)` (tracer/meter providers, OTLP exporter),
  `Logger(ctx)` (slog JSON with correlation fields), span helpers per stage.
- Trace context in jobs: inject `traceparent` into job payload at `Enqueue`; worker extracts
  as parent (links if spans get too deep).
- `deployments/`: otel-collector sidecar config (OTLP gRPC in; `googlecloud` exporter for
  traces, `googlemanagedprometheus` exporter for metrics); Cloud Monitoring dashboard JSON under
  `deployments/terraform/dashboards/`. In Cloud Shell the compose collector uses a console
  exporter.
- Logs: slog JSON on stdout with `logging.googleapis.com/trace` and `spanId` fields so Cloud
  Logging links each line to its trace.

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

## 11. As built

- `internal/observability`: `Setup` (OTLP gRPC to `OTEL_EXPORTER_OTLP_ENDPOINT`, batch span
  processor, periodic metric reader, 2 s export timeout, no retries, parent-based ratio sampler
  from `OTEL_TRACES_SAMPLER_ARG`; no endpoint = no export), `NewLogger` (Cloud Logging JSON:
  `severity`, `message`, `request_id`, `logging.googleapis.com/trace|spanId|trace_sampled`;
  values of `RedactedKeys` such as `content`, `question`, `prompt`, `answer`, `token`,
  `authorization` replaced with `[redacted]`), `Middleware` (request id accepted if it matches
  `[A-Za-z0-9._-]{1,64}`, else generated; echoed as `X-Request-Id`; server span continues an
  inbound `traceparent` and is renamed to the matched route; `http_request_duration_seconds`).
- Metrics (Prometheus names after the collector's suffixing): `http_request_duration_seconds
  {method,route,status}`, `jobs_queue_depth{kind}`, `jobs_queue_age_seconds{kind}`,
  `jobs_attempts_total{kind}`, `jobs_failed_total{kind,outcome}`, `provider_errors_total
  {provider,kind}`, `tokens_total{tenant_id,kind,direction}`, `estimated_cost_usd_total
  {tenant_id}`, plus `rerank_fallbacks_total{reason}` (009) and `ingest_failures_total{reason}`
  (parser failure reasons panel). Token and cost counters are fed by `usage.Recorder` after
  each row is written; prices are list-price estimates in `observability.Prices`.
- Label allowlist: `route, method, status, kind, provider, tenant_id, direction, reason,
  outcome` — the spec's list plus the bounded keys the metrics above need. A single SDK view
  applies it to every instrument (other keys are dropped at runtime), and
  `TestMetricCardinality` is the CI lint.
- Queue gauges read `job_queue_stats()` (`migrations/0009_job_queue_stats.sql`), a new system
  function returning counts per kind only; added to the isolation suite's closed list.
- Spans: ask = `POST /ask` → `retrieve` (`embed question`, `vector search`, `full-text search`,
  `hydrate candidates`, `fusion`) → `rerank` → `llm total` (→ `llm first-token`). The hybrid
  query was split into three statements in one transaction so each leg has a real span.
  Ingestion: `jobs.Enqueue` adds `traceparent` to object payloads; the runner starts
  `job <kind>` as a child of it, backdated to the claim, with a `claim` child, then `extract`,
  `chunk`, `embed batch` (per batch), `index write`.
- Deployment: `deployments/otel-collector/cloudrun.yaml` (sidecar: `googlecloud` traces,
  `googlemanagedprometheus` metrics, memory limiter) stored in Secret Manager and mounted into a
  second container of the `api` Cloud Run service; `local.yaml` + compose `otel-collector`
  (debug exporter). Runtime service accounts get `cloudtrace.agent` and
  `monitoring.metricWriter`. Dashboard: `deployments/terraform/dashboards/platform.json`
  (PromQL tiles for every OBS-NFR-002 panel). The worker and relay sidecars follow when their
  worker pools are added to Terraform.
