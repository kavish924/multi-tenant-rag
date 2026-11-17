# 002 — Document Management: Tasks

## Group 1: Schema

- [x] 1.1 `documents` table + constraints + partial unique index `(tenant_id, sha256) WHERE deleted_at IS NULL` + listing index. (DOC-REQ-004, design §10)

## Group 2: Validation primitives

- [x] 2.1 Signature sniffer: PDF / DOCX / text detection; mismatch vs declared MIME → typed error. (DOC-REQ-002, DOC-AC-002)
- [x] 2.2 Counting-limited reader: aborts at `MaxUploadBytes`. (DOC-REQ-003, DOC-AC-001)

## Group 3: Upload flow

- [x] 3.1 `ObjectStore.Put` call with tenant-prefixed key (interface from `003`; Cloud Storage impl). (DOC-REQ-005)
- [x] 3.2 Transactional create: document row (`uploaded`) + `queue.Enqueue("ingest", ...)` in one tx via `WithTenant`. (DOC-REQ-005, DOC-AC-005)
- [x] 3.3 Dedup handling: catch unique violation → fetch + return existing with `duplicate:true`, no second job. (DOC-REQ-004, DOC-AC-003, DOC-AC-004)
- [x] 3.4 Compensating action on failure: delete stored object (or tombstone row for later sweep). (DOC-REQ-006, DOC-AC-006)

## Group 4: Read/delete handlers

- [x] 4.1 `GET /documents` cursor pagination + status filter; hides deleted by default. (DOC-REQ-007)
- [x] 4.2 `GET /documents/{id}` incl. `error` reason surfaced from job. (DOC-REQ-007)
- [x] 4.3 `DELETE /documents/{id}`: soft delete → status `deleting` + enqueue `cleanup` job (spec `013` contract). (DOC-REQ-008, DOC-AC-008)
- [x] 4.4 Content download refusal for deleted docs (410). (DOC-AC-008)

## Group 5: Tests and verification

- [ ] 5.1 *(written in `tests/integration/documents_test.go` with the fs store, not yet run)* Integration: full upload happy path on Postgres + `fake-gcs-server` testcontainers; tx-failure rollback test.
- [ ] 5.2 *(written in `tests/integration/documents_test.go` with the fs store, not yet run)* Security: cross-tenant GET/DELETE → 404 (DOC-AC-007); forged tenant scope (DOC-AC-009).
- [ ] 5.3 *(concurrent-duplicate test written)* Failure & load: concurrent duplicate uploads → exactly one doc + one job; 50 MB upload p95 timing check (DOC-NFR-002).
- [ ] 5.4 **Milestone 1 exit contribution:** document CRUD + upload validation tests green.
