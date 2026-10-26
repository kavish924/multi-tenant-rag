# 008 — Generation, Streaming & Citations: Requirements

- **Spec ID:** 008 `GEN`
- **Status:** Draft
- **Milestone:** 3 — RAG end to end
- **Depends on:** `specs/007-hybrid-retrieval`
- **Shared context:** `docs/system-design.md §6` (generation, streaming, citations)

## Overview

The read path's answer half: build a grounded prompt from numbered retrieved chunks, stream the
answer over SSE, resolve `[n]` citation markers server-side against this request's retrieval
records only, and record token usage. Client disconnects stop provider spend.

## User stories

- **GEN-US-1:** As a tenant member, I want answers streamed token-by-token so the UX feels instant.
- **GEN-US-2:** As a tenant member, I want citations that map to real passages so I can verify answers.
- **GEN-US-3:** As a platform operator, I want disconnected clients to stop consuming tokens.
- **GEN-US-4:** As a platform operator, I want the model never to invent source identifiers.

## Requirements

### Functional

- **GEN-REQ-001:** `POST /tenants/{id}/ask` with `{"question": "..."}` responds `text/event-stream` with events: `token`, `citation`, `done`, `error` (JSON `data` per event).
- **GEN-REQ-002:** Grounded prompt: chunks numbered 1..N; instructions to answer **only** from them, cite by number, and state when context is insufficient; retrieved text delimited as untrusted data; no secrets in the prompt.
- **GEN-REQ-003:** Streaming uses `http.Flusher`; each event flushed immediately; SSE framing `event:`/`data:` per shared handler.
- **GEN-REQ-004:** Request context propagates into the provider call: client disconnect cancels generation (stops token spend).
- **GEN-REQ-005:** Citation resolution: markers `[n]` are mapped to this request's `hits[n-1]`; out-of-range, non-numeric beyond pattern, and duplicate markers are dropped; output citations contain `chunk_id` + `document_id`.
- **GEN-REQ-006:** Access is re-checked when a citation is later resolved to a document/page (content endpoint authz path).
- **GEN-REQ-007:** On stream end (any outcome), `usage_events` records `kind=generate` with input/output tokens; retrieval embedding usage recorded as `kind=embed`.
- **GEN-REQ-008:** Retrieval failure → 502 before streaming starts; generation failure mid-stream → single `error` event then stream close.
- **GEN-REQ-009:** Zero hits after retrieval → the system instructs the model to answer that context is insufficient (no fabrication from model memory).

### Non-functional

- **GEN-NFR-001:** First token ≤ 1.5 s p95 (provider permitting); SSE flush latency < 10 ms per event.
- **GEN-NFR-002:** Disconnected client ⇒ provider cancellation observed ≤ 1 s.

## Acceptance criteria (EARS)

- **GEN-AC-001:** WHEN a valid ask arrives, THE SYSTEM SHALL stream `token` events followed by `citation` (if any) and `done`.
- **GEN-AC-002:** WHEN the model emits `[2]` and hits contain index 2, THE SYSTEM SHALL emit a citation event resolving to `hits[1]` chunk/document ids.
- **GEN-AC-003:** WHEN the model emits `[99]` (out of range) or repeats a marker, THE SYSTEM SHALL drop it and never surface an unresolvable citation.
- **GEN-AC-004:** WHEN the client disconnects mid-stream, THE SYSTEM SHALL cancel the provider call and stop token spend within GEN-NFR-002.
- **GEN-AC-005:** WHEN retrieval returns zero hits, THE SYSTEM SHALL produce an insufficient-context answer and zero citations.
- **GEN-AC-006:** WHEN retrieval fails, THE SYSTEM SHALL respond 502 without opening the SSE stream.
- **GEN-AC-007:** WHEN a stream ends (success or failure), THE SYSTEM SHALL write the usage event with correct token counts.
- **GEN-AC-008:** WHEN the answer contains prompt-injected text from documents, THE SYSTEM SHALL have passed it only as delimited data (prompt snapshot logged for eval, not for output).
- **GEN-AC-009:** WHEN a citation is later dereferenced, THE SYSTEM SHALL re-verify access (deleted doc ⇒ 410; other tenant ⇒ 404).

## Out of scope

- Guard verdicts on output content (redaction/groundedness checks — `012` plugs into this stream);
  reranking (`009`); quotas enforcement (`011`, but usage recording lands here).
