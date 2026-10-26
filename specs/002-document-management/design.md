# 002 — Document Management: Design

- **Spec ID:** 002 `DOC` · **Milestone:** 1
- **Shared design:** [system-design.md §2 Data model](../../docs/system-design.md#2-data-model), [§3 Ingestion pipeline](../../docs/system-design.md#3-ingestion-pipeline)

## 1. Context and goals

Entry point of the write path. Produces *trustworthy inputs*: validated files, deduplicated
per tenant, and an atomic document-row + job creation so "no job for a document row that
failed to commit" (and vice versa).

## 2. Scope

**In:** upload validation, hashing/dedup, metadata CRUD, soft delete, transactional enqueue call.
**Out:** storage backend (`003`), queue internals (`004`), pipeline stages (`005`), cleanup (`013`).

## 3. Components

- `internal/documents` — service: validate, hash, dedup, persist; `Status` constants.
- `internal/ingestion/extract` — signature sniffing helpers (PDF `%PDF-`, DOCX `PK\x03\x04`, UTF-8 text heuristic).
- `internal/jobs` — `Queue.Enqueue` interface call (impl in `004`).
- `cmd/api` — handlers under `/documents`.

## 4. Data model changes

`documents` table (in `0001_init.sql`): `id, tenant_id, filename, content_type, size_bytes,
object_key, sha256, status CHECK IN ('uploaded','queued','processing','indexed','failed','deleting'),
error, created_at, deleted_at`, `UNIQUE (tenant_id, sha256)`. Index `(tenant_id, created_at DESC)`
for listing.

## 5. API contract

| Method | Path | Notes |
| ------ | ---- | ----- |
| POST | `/documents` | multipart `file`; 202 → `{id, status:"queued"}`; 200 + `duplicate:true` on dedup hit |
| GET | `/documents?limit&cursor&status` | 200 → `{items[], next_cursor}` (excludes deleted unless `include_deleted=true`, admin+) |
| GET | `/documents/{id}` | 200 metadata; 404 other-tenant/unknown; 410 content actions on deleted |
| DELETE | `/documents/{id}` | 202 → status `deleting` (cleanup async in `013`); min role `member` (delete), `admin` (purge in `013`) |

Size error `413 {"error":{"code":"too_large"}}`; type error `415 {"code":"unsupported_type"}`.

## 6. Key flows

**Upload (happy):** stream multipart → enforce size cap while reading (DOC-REQ-003) → sniff
signature → SHA-256 while streaming to object store → `WithTenant` tx: `INSERT document
(status='uploaded')`; on dedup conflict return existing row (no job); else `queue.Enqueue
("ingest", {document_id})` **inside the same tx** → commit → 202.

**Failure path:** tx error → rollback → best-effort `DeleteObject(object_key)` → 500 (client
retries safely: dedup makes retries idempotent).

**Delete (this spec):** authorize → `UPDATE documents SET deleted_at=now(), status='deleting'`
→ enqueue `cleanup` job (`013`) → 202. Retrieval exclusion is enforced in `007`; download
refusal here (DOC-AC-008).

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Body larger than limit | counting reader | 413, nothing stored |
| Signature ≠ declared type | sniff vs header | 415 |
| Duplicate sha256 in tenant | unique violation → select existing | 200 `duplicate:true` |
| Storage put fails | error from `ObjectStore.Put` | 502, no rows |
| Tx commit fails | pgx error | 500 + compensating object delete |

## 8. Security and multi-tenancy

All queries tenant-scoped via `WithTenant` (layer 1+2). Object keys tenant-prefixed (layer 3,
detailed in `003`). Upload size/type limits are the input guard layer for this path; parser
isolation belongs to `005`.

## 9. Testing strategy

- Unit: signature sniffer; size-limit reader; status transitions.
- Integration: upload happy path; dedup same/different tenant (DOC-AC-003/004); list/pagination;
  soft delete metadata behavior (DOC-AC-008).
- Security: cross-tenant GET/DELETE (DOC-AC-007); forged fields (DOC-AC-009).
- Failure: tx rollback leaves no row/job + object deleted (DOC-AC-006).

## 10. Open questions

- Re-upload after delete of a dedup twin: unique index blocks re-insert while soft-deleted row
  exists → **default:** allow re-upload by making the unique index partial
  (`WHERE deleted_at IS NULL`).
