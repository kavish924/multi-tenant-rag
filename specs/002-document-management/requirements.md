# 002 — Document Management: Requirements

- **Spec ID:** 002 `DOC`
- **Status:** Draft
- **Milestone:** 1 — Working Go API
- **Depends on:** `specs/001-tenancy-identity`
- **Shared context:** `docs/system-design.md §2, §3` (data model, ingestion state machine)

## Overview

Document upload and metadata lifecycle: validation (size, MIME, signature), per-tenant
duplicate detection via SHA-256, durable metadata, listing/status tracking, and soft delete.
Upload is the entry of the write path: file stored (via `003`) and document row + ingestion job
created in a single transaction (queue mechanics in `004`).

## User stories

- **DOC-US-1:** As a tenant member, I want to upload PDF/DOCX/TXT files so their content becomes answerable.
- **DOC-US-2:** As a tenant member, I want to see upload and processing status per document so I know what is searchable.
- **DOC-US-3:** As a tenant admin, I want to delete a document so its content stops being used in answers.

## Requirements

### Functional

- **DOC-REQ-001:** `POST /documents` accepts multipart upload; allowed types: `application/pdf`, `application/vnd.openxmlformats-officedocument.wordprocessingml.document` (docx), `text/plain`.
- **DOC-REQ-002:** Type is validated by content signature (magic bytes) in addition to the declared MIME type.
- **DOC-REQ-003:** Maximum upload size is configurable (default 50 MB); enforced before reading the full body into memory.
- **DOC-REQ-004:** SHA-256 is computed while streaming; `UNIQUE (tenant_id, sha256)` makes duplicates per tenant impossible.
- **DOC-REQ-005:** Upload stores the object (via `003`), then in **one transaction** inserts the document row (status `uploaded`) and enqueues an ingestion job (status `queued`).
- **DOC-REQ-006:** If any part of the transaction fails, neither the document row nor the job persists; the stored object is deleted (best-effort compensating action).
- **DOC-REQ-007:** `GET /documents` lists tenant documents with pagination; `GET /documents/{id}` returns metadata incl. status and error reason.
- **DOC-REQ-008:** `DELETE /documents/{id}` performs authorization + soft delete (sets `deleted_at`, status `deleting`); physical cleanup is spec `013`.
- **DOC-REQ-009:** Deleted documents' metadata remains listable (filtered by default) but their content is never retrievable or searchable (enforced again in `007`).

### Non-functional

- **DOC-NFR-001:** Uploads are streamed to object storage (bounded memory, no full-file buffering in RAM).
- **DOC-NFR-002:** `POST /documents` responds after the transaction commits (not after processing): p95 < 1 s for a 50 MB file on local MinIO.

## Acceptance criteria (EARS)

- **DOC-AC-001:** WHEN an upload exceeds the size limit, THE SYSTEM SHALL respond 413 and store nothing.
- **DOC-AC-002:** WHEN the declared MIME type disagrees with the detected signature, THE SYSTEM SHALL respond 415 and store nothing.
- **DOC-AC-003:** WHEN a tenant re-uploads a file with an identical SHA-256, THE SYSTEM SHALL return the existing document (200, `duplicate: true`) and not enqueue a second job.
- **DOC-AC-004:** WHEN two tenants upload the same file content, THE SYSTEM SHALL store two independent documents (dedup is per-tenant).
- **DOC-AC-005:** IF the document row insert succeeds, THEN an ingestion job for it SHALL exist in the same commit.
- **DOC-AC-006:** WHEN the transaction fails, THE SYSTEM SHALL leave no document row and no job; the object SHALL be deleted or scheduled for cleanup.
- **DOC-AC-007:** WHEN a member of tenant B requests document id of tenant A, THE SYSTEM SHALL respond 404.
- **DOC-AC-008:** WHEN a document is soft-deleted, `GET /documents/{id}` SHALL still return metadata (with `status: deleting`) for members, but content download SHALL be refused (410).
- **DOC-AC-009:** WHEN a `member` of another tenant uploads into tenant A by forging fields, THE SYSTEM SHALL scope the document to the verified tenant only.

## Out of scope

- Ingestion processing itself (`005`); object storage mechanics (`003`); queue mechanics (`004`);
  physical cleanup (`013`).
