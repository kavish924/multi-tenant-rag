# 003 — Object Storage: Requirements

- **Spec ID:** 003 `STO`
- **Status:** In progress
- **Milestone:** 2 — Async ingestion
- **Depends on:** `specs/001-tenancy-identity`, `specs/002-document-management`
- **Shared context:** `docs/tech-stack.md` (files), `docs/system-design.md §1` (storage isolation layer)

## Overview

Google Cloud Storage behind a small Go interface. Every environment (dev/test/prod) uses a real
Cloud Storage bucket; tests use the `fake-gcs-server` emulator through the same client. Keys are
tenant-prefixed (isolation layer 3) and any signed URL is issued only after an authorization
check.

## User stories

- **STO-US-1:** As a tenant member, I want to download the original of a document I can access.
- **STO-US-2:** As a developer, I want one storage interface so tests (emulator) and every environment (Cloud Storage) behave identically.
- **STO-US-3:** As a platform operator, I want object access to be impossible to reach cross-tenant, even via shared URLs.

## Requirements

### Functional

- **STO-REQ-001:** Interface `ObjectStore`: `Put(ctx, key, r)`, `Get(ctx, key)`, `Delete(ctx, key)`, `Presign(ctx, key, ttl)`.
- **STO-REQ-002:** Keys are `tenants/{tenant_id}/documents/{document_id}` — no exceptions.
- **STO-REQ-003:** One implementation using `cloud.google.com/go/storage`, configured via env (`GCS_BUCKET`); authenticates with the runtime service account (no keys); `STORAGE_EMULATOR_HOST` points it at `fake-gcs-server` in tests.
- **STO-REQ-004:** `GET /documents/{id}/content` authorizes (tenant membership), then streams the object server-side (no signed URL needed for small files) or issues a short-TTL V4 signed URL (signed via IAM `signBlob` with the API service account).
- **STO-REQ-005:** The worker fetches originals with its service account, never via signed URLs.
- **STO-REQ-007:** Buckets use uniform bucket-level access with public access prevention enforced; only the `api` and `worker` service accounts hold object permissions, on their environment's bucket only.
- **STO-REQ-006:** `Delete` is idempotent (missing object ≠ error).

### Non-functional

- **STO-NFR-001:** Downloads stream (no full buffering); memory bounded regardless of file size.

## Acceptance criteria (EARS)

- **STO-AC-001:** WHEN a member requests content of a document in their tenant, THE SYSTEM SHALL return the bytes streamed.
- **STO-AC-002:** WHEN a non-member or anonymous caller requests content, THE SYSTEM SHALL respond 404/401 before any storage access.
- **STO-AC-003:** WHEN a signed URL is issued, THE SYSTEM SHALL have verified requester membership first, and the TTL SHALL be ≤ 5 minutes.
- **STO-AC-004:** WHEN any code path constructs a key, THE SYSTEM SHALL only accept the `tenants/{tenant_id}/...` form (key builder is the single source).
- **STO-AC-005:** WHEN a signed URL for tenant A's object is replayed by tenant B, THE SYSTEM SHALL deny access once the TTL has expired, SHALL deny any unsigned or altered-key request (public access prevention), and the test shall prove the URL grants nothing beyond that one object for its TTL.
- **STO-AC-006:** WHEN `Delete` is called for an already-deleted key, THE SYSTEM SHALL return success.

## Out of scope

- Virus/malware scanning hooks (guardrail spec `012` may integrate later).
- Bucket lifecycle/retention automation (`013`).
