# 013 — Deletion & Cleanup: Design

- **Spec ID:** 013 `DEL` · **Milestone:** 5
- **Shared design:** [system-design.md §3](../../docs/system-design.md#3-ingestion-pipeline) (deletion), [§9](../../docs/system-design.md#9-production-security-and-reliability) (accidental deletion)

## 1. Context and goals

Make deletion complete, race-free, and idempotent. Deletion races are an explicit red-team
category, so the in-flight-job interaction is designed, not improvised.

## 2. Scope

**In:** cleanup job kind, purge operation, sweeper, idempotent outbox consumer reference.
**Out:** storage lifecycle rules (optional optimization), backup/restore.

## 3. Components

- `internal/documents`: delete service (from `002`) + purge service.
- `internal/ingestion/cleanup`: stage implementations for the `cleanup` job kind.
- Sweeper: periodic job (`run_at` scheduled by relay-less cron loop in worker). It spans
  tenants, so it finds work via the `sweep_candidates(limit)` system function (`TEN-REQ-010`,
  returns `(tenant_id, document_id)` only; reads `documents` via a `system_access` policy scoped
  `TO app_system`) and does each removal inside `WithTenant`.

## 4. Data model changes

None structural. Status lifecycle gains terminal `deleted`. Retention via config.

## 5. API contract

- `DELETE /tenants/{id}/documents/{docId}` (from `002`) → 202.
- `POST /tenants/{id}/purge` (role `owner`; operator double-confirmation header in tooling) → 202 + purge job.

## 6. Key flows

**Delete:** soft-delete (`deleting`) → outbox `document.deleting` + `cleanup` job (idempotency key
`cleanup:{document_id}`) → job: `DELETE FROM chunks WHERE document_id=$1` (explicit, batched)
→ cache purge → object delete → status `deleted`. The document row is only soft-deleted, so
`ON DELETE CASCADE` does not fire here; it remains as a safety net for the sweeper's eventual
hard delete. The job asserts zero rows at the end.

**Race handling:** ordering comes from the **document row lock**, not from the queue — the
ingest job and the `cleanup:{document_id}` job are different jobs with different idempotency
keys, so nothing in the queue orders them.

| Interleaving | What happens |
| ------------ | ------------ |
| Delete during extract/chunk/embed | Index's `FOR UPDATE` read sees `deleted_at` → no chunks (ING-AC-005) |
| Delete while Index holds the lock | Delete waits; Index commits chunks; Delete commits + enqueues cleanup; cleanup deletes them |
| Delete commits, Index then takes the lock | Index re-reads the latest row, sees `deleted_at` → no chunks (ING-AC-009) |
| Ingest retried after delete | job start sees `deleted_at` → completes as cancelled (ING-REQ-010) |

Final invariant checked at end of cleanup (`SELECT count(*) FROM chunks WHERE document_id=$1` = 0);
non-zero ⇒ job fails and retries (it indicates a bug, so it also alerts).

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Object delete fails | Cloud Storage error | retry ladder (DEL-AC-004) |
| Chunks remain post-cleanup | invariant check | job fails → retry; alert after N attempts |
| Purge partial failure | per-document error list | resume from failed set (idempotent) |

## 8. Security and multi-tenancy

Authorization matrix enforced at endpoints (DEL-AC-007). Purge operates strictly inside the
tenant context; objects addressed only via `DocumentKey` (no key forging). Deleted content can
never reappear in search or citations (red-team "deletion races" pass condition).

## 9. Testing strategy

- Integration: delete → immediate exclusion (DEL-AC-001); cleanup invariant (AC-002); retry
  after injected object-delete failure (AC-004); duplicate event idempotency (AC-005); purge (AC-006).
- Failure/race: delete-during-embed and delete-during-index scheduled interleavings (AC-003).
- Sweeper: retention + orphan removal (AC-008).

## 10. Open questions

- Cache layer shape (answer cache exists only if `009+` add it) → **default:** cache purge is a
  no-op interface today; concretized when caching lands (any cache key MUST include tenant).
