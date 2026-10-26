# 009 — Reranking (Optional Stage): Requirements

- **Spec ID:** 009 `RER`
- **Status:** Draft
- **Milestone:** 3 — RAG end to end
- **Depends on:** `specs/007-hybrid-retrieval`, `specs/008-generation-citations`
- **Shared context:** `docs/system-design.md §5` (rerank note: measure before keeping)

## Overview

An optional stage between fusion and prompt assembly: a cross-encoder or LLM-based reranker
narrows top-50 fused candidates to top-8. It is off by default, must degrade gracefully, and
its latency/cost/quality trade-off must be measured (via `014`) before it stays enabled.

## User stories

- **RER-US-1:** As a tenant member, I want the most relevant passages in the prompt so answers are better.
- **RER-US-2:** As a platform operator, I want reranking to fail open (fall back to fusion order) so an outage doesn't break asking.
- **RER-US-3:** As a developer, I want reranking behind an interface so cross-encoder and LLM implementations are swappable.

## Requirements

### Functional

- **RER-REQ-001:** `Reranker.Rerank(ctx, tenantID, question, hits, keep) ([]Hit, error)`; config: `enabled`, `provider` (`cross_encoder` | `llm`), `keep` (default 8), `timeout` (default 800 ms).
- **RER-REQ-002:** Default `enabled=false`; enabling is per-deployment config, not per-request.
- **RER-REQ-003:** On reranker error or timeout, the system falls back to the fused order (first `keep` hits) and logs/metrics the degradation — never fails the request.
- **RER-REQ-004:** Every rerank call records `usage_events(kind='rerank')` with token counts (LLM impl) or item counts (cross-encoder impl) so cost is attributable.
- **RER-REQ-005:** Reranking preserves hit identity: a reranked hit keeps its `chunk_id`/`document_id` (citation integrity).
- **RER-REQ-006:** Rerank runs inside the tenant context; the provider request contains only this request's hits for this tenant.

### Non-functional

- **RER-NFR-001:** Added latency p95 ≤ 800 ms; when exceeded, the timeout path (fall back) applies.
- **RER-NFR-002:** Quality uplift is measurable: eval harness reports delta (nDCG / answer faithfulness) with rerank on vs off.

## Acceptance criteria (EARS)

- **RER-AC-001:** WHEN reranking is disabled, THE SYSTEM SHALL use the fused order with no rerank calls.
- **RER-AC-002:** WHEN the reranker times out or errors, THE SYSTEM SHALL proceed with fused-order hits and record a `rerank_fallback` metric event.
- **RER-AC-003:** WHEN reranking succeeds, THE SYSTEM SHALL return at most `keep` hits, ordered by reranker score, identity-preserved.
- **RER-AC-004:** WHEN a rerank call happens, THE SYSTEM SHALL write a `rerank` usage event.
- **RER-AC-005:** WHEN evaluating on/off, THE SYSTEM SHALL produce comparable measurements from `014` (report artifact exists).

## Out of scope

- Cross-encoder model hosting (provider-agnostic interface; local model optional later).
- Query rewriting/expansion (evaluation may propose; separate spec).
