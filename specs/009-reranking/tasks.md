# 009 — Reranking: Tasks

## Group 1: Interface and implementations

- [ ] 1.1 `Reranker` interface + config wiring (disabled by default). (RER-REQ-001/002, RER-AC-001)
- [ ] 1.2 Cross-encoder impl (hosted scoring endpoint). (RER-REQ-001)
- [ ] 1.3 LLM listwise impl with strict output parsing → fallback on malformed output. (RER-REQ-001)

## Group 2: Resilience and accounting

- [ ] 2.1 Timeout wrapper + fail-open fallback + `rerank_fallback` metric hook. (RER-REQ-003, RER-AC-002, RER-NFR-001)
- [ ] 2.2 `rerank` usage events (tokens/items). (RER-REQ-004, RER-AC-004)
- [ ] 2.3 Identity-preservation test: reranked hits keep chunk/document ids. (RER-REQ-005, RER-AC-003)

## Group 3: Integration and measurement

- [ ] 3.1 Insert stage into ask path between fusion and prompt build. (RER-REQ-001)
- [ ] 3.2 A/B eval artifact via `014` harness (on vs off). (RER-AC-005, RER-NFR-002)
