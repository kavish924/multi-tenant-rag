# 007 — Hybrid Retrieval (RRF): Requirements

- **Spec ID:** 007 `RET`
- **Status:** Draft
- **Milestone:** 3 — RAG end to end
- **Depends on:** `specs/005-ingestion-pipeline`
- **Shared context:** `docs/system-design.md §5` (hybrid retrieval)

## Overview

Tenant-scoped hybrid search: semantic vectors (pgvector, HNSW) + full-text (`tsvector`, GIN)
fused with Reciprocal Rank Fusion (k=60). Excludes soft-deleted/`deleting` documents, caps
chunks per document, and returns ranked hits with citation-ready metadata.

## User stories

- **RET-US-1:** As a tenant member, I want questions to find passages by meaning **and** by exact terms (names, error codes) so answers are well grounded.
- **RET-US-2:** As a tenant member, I want deleted document content to never appear in results.
- **RET-US-3:** As a developer, I want a single `Retriever.Search` interface so ranking strategies can change without touching generation.

## Requirements

### Functional

- **RET-REQ-001:** `Retriever.Search(ctx, tenantID, question, limit)` returns hits: `chunk_id`, `document_id`, `content`, `page`, `heading`, `score`, `rank`.
- **RET-REQ-002:** Vector leg: top-50 by cosine distance, tenant-scoped (per shared `hybrid.sql`).
- **RET-REQ-003:** Full-text leg: top-50 by `ts_rank_cd` with `websearch_to_tsquery('english', question)`, tenant-scoped.
- **RET-REQ-004:** Fusion: RRF `score = Σ 1/(60 + rank)` over both legs; order by score desc; final `LIMIT` parameter (default 50 in, 8 out at generation).
- **RET-REQ-005:** Results join `documents` and exclude `deleted_at IS NOT NULL` or `status='deleting'`.
- **RET-REQ-006:** Per-document chunk cap (default 5) so one file cannot dominate results.
- **RET-REQ-007:** Filtered-vector-search small-tenant recall is validated: test with the smallest tenant; if pgvector supports iterative index scans, enable; otherwise document the limitation and mitigate (over-fetch factor).

### Non-functional

- **RET-NFR-001:** p95 search latency ≤ 300 ms on a 1M-chunk corpus with realistic tenant sizes.
- **RET-NFR-002:** Zero cross-tenant leakage under adversarial probing (property: impossible by construction, proven by isolation tests).

## Acceptance criteria (EARS)

- **RET-AC-001:** WHEN a tenant searches, THE SYSTEM SHALL return only chunks whose `tenant_id` equals the authenticated tenant — including when candidate ids from another tenant are probed directly.
- **RET-AC-002:** WHEN a document is soft-deleted or `deleting`, THE SYSTEM SHALL return none of its chunks.
- **RET-AC-003:** WHEN one document contains most of the corpus matches, THE SYSTEM SHALL return at most the per-document cap from it.
- **RET-AC-004:** WHEN the question matches by exact identifier only (e.g. an error code), THE SYSTEM SHALL rank the containing chunk in the top-10 (full-text leg proof).
- **RET-AC-005:** WHEN the question is semantically similar but lexically different, THE SYSTEM SHALL rank the containing chunk in the top-10 (vector leg proof).
- **RET-AC-006:** WHEN the question is empty or whitespace, THE SYSTEM SHALL respond 400 without calling providers.
- **RET-AC-007:** WHEN fusion runs, THE SYSTEM SHALL score a chunk appearing in both legs higher than the same chunk in one leg (RRF math test).
- **RET-AC-008:** WHEN `limit` exceeds the candidate pool, THE SYSTEM SHALL return all valid candidates ordered by score.

## Out of scope

- Reranker stage (`009`); answer generation (`008`); embedding of the question happens here but
  its provider config is shared with `005`.
