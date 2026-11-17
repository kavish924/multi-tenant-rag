# 010 — Observability: Tasks

## Group 1: Plumbing

- [ ] 1.1 *(SDK setup, collector configs, compose service and Cloud Run sidecar in Terraform done; not yet applied on GCP)* `internal/observability.Setup` for api/worker; OTLP exporter; collector sidecar on Cloud Run (Cloud Trace + Managed Prometheus exporters); compose `otel-collector` in Cloud Shell. (OBS-REQ-001)
- [x] 1.2 Request middleware: request id + root span + `X-Request-Id` echo. (OBS-REQ-001, OBS-AC-001)
- [x] 1.3 Trace context injection into job payloads; worker parent extraction. (OBS-REQ-003, OBS-AC-002)

## Group 2: Spans and metrics

- [x] 2.1 Query-path spans (embed/vector/fts/fusion/rerank/llm first-token/total). (OBS-REQ-002, OBS-AC-001)
- [x] 2.2 Ingestion-path spans (claim/extract/chunk/embed batch/index). (OBS-REQ-003)
- [x] 2.3 Metric instrumentations per OBS-REQ-004 (incl. queue depth/age gauges, token/cost counters fed by `usage_events`). (OBS-REQ-004, OBS-AC-005)
- [x] 2.4 Metric label allowlist enforcement + CI lint. (OBS-REQ-005, OBS-AC-003)

## Group 3: Logs and dashboards

- [x] 3.1 slog JSON wrapper: correlation ids; redaction guard (no doc text/secrets) + automated log-scan test. (OBS-REQ-006, OBS-AC-004)
- [ ] 3.2 *(dashboard JSON + `google_monitoring_dashboard` written; not yet applied)* Cloud Monitoring dashboards per OBS-NFR-002 (Terraform, JSON in repo). (OBS-NFR-002)
- [x] 3.3 Collector-down resilience test (non-blocking). (OBS-AC-006)

## Group 4: Verification

- [ ] 4.1 *(pending a load environment)* Overhead benchmark < 5% p95. (OBS-NFR-001)
- [ ] 4.2 *(needs the deployed dev environment)* **Milestone 4 exit contribution:** demonstrate explaining a slow + a failed request purely from telemetry.
