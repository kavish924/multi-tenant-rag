# 010 — Observability: Tasks

## Group 1: Plumbing

- [ ] 1.1 `internal/observability.Setup` for api/worker; OTLP exporter; compose `otel-collector`, `prometheus`, `grafana` services. (OBS-REQ-001)
- [ ] 1.2 Request middleware: request id + root span + `X-Request-Id` echo. (OBS-REQ-001, OBS-AC-001)
- [ ] 1.3 Trace context injection into job payloads; worker parent extraction. (OBS-REQ-003, OBS-AC-002)

## Group 2: Spans and metrics

- [ ] 2.1 Query-path spans (embed/vector/fts/fusion/rerank/llm first-token/total). (OBS-REQ-002, OBS-AC-001)
- [ ] 2.2 Ingestion-path spans (claim/extract/chunk/embed batch/index). (OBS-REQ-003)
- [ ] 2.3 Metric instrumentations per OBS-REQ-004 (incl. queue depth/age gauges, token/cost counters fed by `usage_events`). (OBS-REQ-004, OBS-AC-005)
- [ ] 2.4 Metric label allowlist enforcement + CI lint. (OBS-REQ-005, OBS-AC-003)

## Group 3: Logs and dashboards

- [ ] 3.1 slog JSON wrapper: correlation ids; redaction guard (no doc text/secrets) + automated log-scan test. (OBS-REQ-006, OBS-AC-004)
- [ ] 3.2 Grafana dashboards per OBS-NFR-002 (JSON provisioned in repo). (OBS-NFR-002)
- [ ] 3.3 Collector-down resilience test (non-blocking). (OBS-AC-006)

## Group 4: Verification

- [ ] 4.1 Overhead benchmark < 5% p95. (OBS-NFR-001)
- [ ] 4.2 **Milestone 4 exit contribution:** demonstrate explaining a slow + a failed request purely from telemetry.
