# 005 — Ingestion Pipeline: Requirements

- **Spec ID:** 005 `ING`
- **Status:** Draft
- **Milestone:** 2 — Async ingestion
- **Depends on:** `specs/002-document-management`, `003-object-storage`, `004-job-queue`
- **Shared context:** `docs/system-design.md §3` (ingestion pipeline state machine)

## Overview

Processes an uploaded document through idempotent stages — Extract → Chunk → Embed → Index —
so a crash and retry at any point cannot corrupt the document. Parsers run in isolation with
resource limits (they are a common attack surface). Final state: `indexed`, or `failed` with a
recorded reason.

## User stories

- **ING-US-1:** As a tenant member, I want my uploaded document to become searchable automatically.
- **ING-US-2:** As a tenant member, I want a clear failure reason when processing fails.
- **ING-US-3:** As a platform operator, I want hostile/malformed files to fail safely without harming workers.
- **ING-US-4:** As a developer, I want stage idempotency so retries and reprocessing are safe by construction.

## Requirements

### Functional

- **ING-REQ-001 (Extract):** converts PDF/DOCX/TXT to text with per-chunk metadata: page number and nearest preceding heading.
- **ING-REQ-002:** Parsers execute in isolation: separate process (or equivalent hard boundary) with memory limit, CPU time limit, wall-clock timeout, and output size cap; a parser failure kills only that execution.
- **ING-REQ-003 (Chunk):** target 300–800 tokens per chunk, 10–15% overlap, preferring paragraph and heading boundaries; each chunk records `ordinal`, `token_count`, `page`, `heading`.
- **ING-REQ-004:** Chunk ordinals are stable across reprocessing: same input ⇒ same ordinals ⇒ upserts reuse rows (`UNIQUE (document_id, ordinal)`).
- **ING-REQ-005 (Embed):** chunks are embedded in batches; every returned vector's dimension is validated against the configured model dimension before writing.
- **ING-REQ-006:** The embedding model name is recorded per chunk (`embed_model`) so re-embedding under a model change is possible.
- **ING-REQ-007:** Rate limits from the embedding provider are respected with bounded retries (queue-level backoff applies above this).
- **ING-REQ-008 (Index):** chunks + vectors are written in **one transaction** that re-checks the document was not deleted meanwhile; on success document status → `indexed`.
- **ING-REQ-009:** Any stage failure sets document `failed` with `error` reason; the job fails per queue retry semantics; document may re-enter `processing` on retry.
- **ING-REQ-010:** Stages are idempotent: rerunning the job from any point produces the same final rows (upsert semantics everywhere).

### Non-functional

- **ING-NFR-001:** A 50 MB PDF finishes within 5 min at p95 (embed provider latency excluded, mocked at 100 ms/batch).
- **ING-NFR-002:** Worker memory stays bounded: ≤ 256 MB RSS per worker under the standard load profile.

## Acceptance criteria (EARS)

- **ING-AC-001:** WHEN a document is processed, THE SYSTEM SHALL produce chunks with `ordinal`, `token_count`, `page`, `heading`, `content`, `embed_model`, `embedding`.
- **ING-AC-002:** WHEN a parser exceeds its memory/time/output limits, THE SYSTEM SHALL fail that job with reason `parser_limit_exceeded` and the worker SHALL continue serving other jobs.
- **ING-AC-003:** WHEN an embedding response has a wrong-dimension vector, THE SYSTEM SHALL fail with reason `embedding_dimension_mismatch` before any DB write.
- **ING-AC-004:** WHEN the job is retried after a crash between Embed and Index, THE SYSTEM SHALL upsert the same `(document_id, ordinal)` rows without duplicates.
- **ING-AC-005:** WHEN the document is soft-deleted before the Index transaction, THE SYSTEM SHALL write no chunks and mark the job succeeded-as-cancelled (document stays `deleting`).
- **ING-AC-006:** WHEN the pipeline completes, THE SYSTEM SHALL set status `indexed`; on terminal failure, `failed` + human-readable `error`.
- **ING-AC-007:** WHEN a malicious file (e.g. decompression bomb, malformed PDF) is processed, THE SYSTEM SHALL fail safely with a reason and no worker crash.
- **ING-AC-008:** WHEN processing finishes (either way), THE SYSTEM SHALL emit the corresponding event via the outbox (`006`): `document.indexed` or `document.failed`.

## Out of scope

- OCR for scanned PDFs; spreadsheet formats; OCR/ASR pipeline (future).
- Reranking/model quality concerns (`007`/`009`).
