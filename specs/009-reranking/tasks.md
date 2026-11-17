# 009 — Reranking: Tasks

## Group 1: Interface and implementations

- [x] 1.1 `Reranker` interface + config wiring (disabled by default). (RER-REQ-001/002, RER-AC-001)
- [ ] 1.2 *(client for the Vertex AI Ranking API shape done and tested against a stub; OAuth credentials land with the GCP work)* Cross-encoder impl (hosted scoring endpoint). (RER-REQ-001)
- [x] 1.3 LLM listwise impl with strict output parsing → fallback on malformed output. (RER-REQ-001)

## Group 2: Resilience and accounting

- [x] 2.1 Timeout wrapper + fail-open fallback + `rerank_fallback` metric hook. (RER-REQ-003, RER-AC-002, RER-NFR-001)
- [ ] 2.2 *(done in code; DB assertion in `internal/reranking/usage_integration_test.go`, not yet run)* `rerank` usage events (tokens/items). (RER-REQ-004, RER-AC-004)
- [x] 2.3 Identity-preservation test: reranked hits keep chunk/document ids. (RER-REQ-005, RER-AC-003)

## Group 3: Integration and measurement

- [x] 3.1 Insert stage into ask path between fusion and prompt build. (RER-REQ-001)
- [ ] 3.2 *(needs the 014 harness)* A/B eval artifact via `014` harness (on vs off). (RER-AC-005, RER-NFR-002)
