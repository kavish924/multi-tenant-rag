# 013 — Deletion & Cleanup: Requirements

- **Spec ID:** 013 `DEL`
- **Status:** Draft
- **Milestone:** 5 — Release-quality capstone
- **Depends on:** `specs/002`, `004`, `005`, `006`
- **Shared context:** `docs/system-design.md §3` (deletion note), `§9` (accidental deletion)

## Overview

Deleting a document means more than removing metadata: chunks, vectors, cached content, the
stored object, and derived artifacts must be cleaned per retention policy — and in-flight jobs
must never reinsert deleted content after cleanup finishes.

## User stories

- **DEL-US-1:** As a tenant admin, I want to delete a document so its content is never answerable again.
- **DEL-US-2:** As a compliance owner, I want a tenant data purge that removes all content and derived artifacts.
- **DEL-US-3:** As a platform operator, I want cleanup retries to be safe and idempotent.

## Requirements

### Functional

- **DEL-REQ-001:** Delete marks the document `deleting` + `deleted_at` immediately (soft delete) and enqueues a `cleanup` job — content becomes non-retrievable at once (retrieval already excludes `deleting`).
- **DEL-REQ-002:** Cleanup job: delete chunks (cascade covers rows; explicitly delete vectors via same rows), any cached answer/embedding entries for the document, then the stored object (`ObjectStore.Delete`, idempotent).
- **DEL-REQ-003:** In-flight safety: the ingestion Index stage re-checks `deleted_at IS NULL` inside its transaction (`ING-AC-005`); cleanup runs **after** in-flight job leases expire or is retried until no chunks exist — final invariant: no chunk rows remain.
- **DEL-REQ-004:** After successful cleanup, document status moves to `deleted` (metadata retained per retention policy; default: 30 days, configurable).
- **DEL-REQ-005:** Tenant purge (admin+): soft-delete all tenant documents, purge memberships? — **no**: purge content + objects + usage rows; tenant/user metadata anonymized or retained per policy flag.
- **DEL-REQ-006:** The purge/cleanup consumer is the idempotency reference implementation for outbox consumers: `document.deleting` events dedupe on `event_id` (per `006`).
- **DEL-REQ-007:** Retention policy config: `RETENTION_DAYS` for deleted-metadata retention; a sweeper removes expired metadata rows + orphans (objects with no document row).

### Non-functional

- **DEL-NFR-001:** After DELETE returns 202, search results never include the document's content within 1 s (exclusion is immediate; cleanup is async).
- **DEL-NFR-002:** Cleanup of a 5k-chunk document completes < 60 s including object deletion.

## Acceptance criteria (EARS)

- **DEL-AC-001:** WHEN an authorized user deletes a document, THE SYSTEM SHALL exclude it from search and content download immediately.
- **DEL-AC-002:** WHEN cleanup finishes, THE SYSTEM SHALL have zero chunk rows, no cached entries, and no stored object for the document.
- **DEL-AC-003:** WHEN an ingestion job is mid-flight during deletion, THE SYSTEM SHALL never leave deleted content in the index after cleanup completes (race test: delete during embed → final state has no chunks).
- **DEL-AC-004:** WHEN cleanup partially fails (e.g. object delete), THE SYSTEM SHALL retry via the job ladder until the invariant (AC-002) holds, then mark `deleted`.
- **DEL-AC-005:** WHEN a duplicate `document.deleting` event is consumed, THE SYSTEM SHALL perform no duplicate side effects (idempotency reference test).
- **DEL-AC-006:** WHEN a tenant purge completes, THE SYSTEM SHALL leave no content chunks, no objects, and no usage rows for that tenant.
- **DEL-AC-007:** WHEN deletion is requested by a non-admin member without rights, THE SYSTEM SHALL respond per the authz matrix (403/404).
- **DEL-AC-008:** WHEN the sweeper runs, THE SYSTEM SHALL remove metadata past retention and orphan objects, with a report metric.

## Out of scope

- Legal hold workflows; cross-tenant "right to erasure" tooling beyond tenant purge; backup restore procedures (documented in runbook instead).
