# 002 — Document Management: Tasks

## Group 1: Schema

- [ ] 1.1 `documents` table + constraints + partial unique index `(tenant_id, sha256) WHERE deleted_at IS NULL` + listing index. (DOC-REQ-004, design §10)

## Group 2: Validation primitives

- [ ] 2.1 Signature sniffer: PDF / DOCX / text detection; mismatch vs declared MIME → typed error. (DOC-REQ-002, DOC-AC-002)
- [ ] 2.2 Counting-limited reader: aborts at `MaxUploadBytes`. (DOC-REQ-003, DOC-AC-001)

## Group 3: Upload flow

- [ ] 3.1 `ObjectStore.Put` call with tenant-prefixed key (interface from `003`; MinIO impl). (DOC-REQ-005)
- [ ] 3.2 Transactional create: document row (`uploaded`) + `queue.Enqueue("ingest", ...)` in one tx via `WithTenant`. (DOC-REQ-005, DOC-AC-005)
- [ ] 3.3 Dedup handling: catch unique violation → fetch + return existing with `duplicate:true`, no second job. (DOC-REQ-004, DOC-AC-003, DOC-AC-004)
- [ ] 3.4 Compensating action on failure: delete stored object (or tombstone row for later sweep). (DOC-REQ-006, DOC-AC-006)

## Group 4: Read/delete handlers

- [ ] 4.1 `GET /documents` cursor pagination + status filter; hides deleted by default. (DOC-REQ-007)
- [ ] 4.2 `GET /documents/{id}` incl. `error` reason surfaced from job. (DOC-REQ-007)
- [ ] 4.3 `DELETE /documents/{id}`: soft delete → status `deleting` + enqueue `cleanup` job (spec `013` contract). (DOC-REQ-008, DOC-AC-008)
- [ ] 4.4 Content download refusal for deleted docs (410). (DOC-AC-008)

## Group 5: Tests and verification

- [ ] 5.1 Integration: full upload happy path on MinIO testcontainer; tx-failure rollback test.
- [ ] 5.2 Security: cross-tenant GET/DELETE → 404 (DOC-AC-007); forged tenant scope (DOC-AC-009).
- [ ] 5.3 Failure & load: concurrent duplicate uploads → exactly one doc + one job; 50 MB upload p95 timing check (DOC-NFR-002).
- [ ] 5.4 **Milestone 1 exit contribution:** document CRUD + upload validation tests green.
