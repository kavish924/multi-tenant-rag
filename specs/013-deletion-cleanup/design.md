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
- Sweeper: periodic job (`run_at` scheduled by relay-less cron loop in worker).

## 4. Data model changes

None structural. Status lifecycle gains terminal `deleted`. Retention via config.

## 5. API contract

- `DELETE /tenants/{id}/documents/{docId}` (from `002`) → 202.
- `POST /tenants/{id}/purge` (role `owner`; operator double-confirmation header in tooling) → 202 + purge job.

## 6. Key flows

**Delete:** soft-delete (`deleting`) → outbox `document.deleting` + `cleanup` job (idempotency key
`cleanup:{document_id}`) → job: delete chunks tx → cache purge → object delete → status
`deleted`. Chunks deletion relies on `ON DELETE CASCADE` from documents, but the job asserts
zero rows explicitly (belt and braces).

**Race handling:** delete during embed ⇒ Index pre-check cancels (ING-AC-005); delete during
Index commit ⇒ chunks landed, but cleanup job deletes them after; ordering guaranteed because
cleanup is enqueued in the delete tx and the queue serializes per idempotency key. Final
invariant checked at end of cleanup (`SELECT count(*) FROM chunks WHERE document_id=$1` = 0).

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Object delete fails | S3 error | retry ladder (DEL-AC-004) |
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
