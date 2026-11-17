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

## 11. As built

- `internal/ingestion/cleanup.Cleaner` (job kind `cleanup`, key `cleanup:{document_id}`):
  load (a missing row means already swept; a live document is a permanent failure) → delete
  chunks in batches of 1,000, each in its own short transaction → `CachePurger` (no-op
  `NoCache` today) → `ObjectStore.Delete(DocumentKey(tenant, doc))` (key never read from the row)
  → under the row lock: assert zero chunks (else retry + `ALERT` log), set `deleted`, emit
  `document.deleted`. Every step is idempotent, so the job retry ladder covers partial failures.
- `cleanup.Purger` (job kind `purge`): soft-deletes every live document exactly like a user
  delete (event + cleanup job), then cleans every `deleting` document inline, so a finished
  purge means no chunks and no objects; then deletes `usage_events` and
  `tenant_usage_counters` and emits `tenant.purged`. A failed document fails the job and the
  retry resumes. Tenant row, users and memberships are **retained** (anonymization is out of
  scope). `POST /tenants/{tenant}/purge`: owner only, requires `X-Confirm-Purge: <tenant id>`,
  returns 202 `{job_id}`. Document delete stays member+ (spec 002).
- `cleanup.DeletingConsumer`: the idempotent-consumer reference (DEL-REQ-006) — on
  `document.deleting` it records the event in `consumed_events` and ensures the cleanup job in
  the same tenant transaction. It is a `messaging.Publisher`; production enqueues cleanup in the
  delete transaction itself, so it is a safety net.
- `cleanup.Sweeper` (in `cmd/worker`, one active via an advisory lock, every `SWEEP_INTERVAL`
  = 1h): hard-deletes `deleted` rows past `RETENTION_DAYS` (30) through two new system
  functions, `sweep_candidates(retention, limit)` and `sweep_delete_document(id, retention)`
  — the app role still has no `DELETE` on `documents`, and the function only removes cleaned,
  expired rows. Orphans: stores implementing `objectstore.Lister` (the fs store; GCS with the
  GCP work) are listed; objects older than `ORPHAN_GRACE` (1h, protecting in-flight uploads)
  with no document row in their own tenant are deleted. `sweeper_removed_total{kind}`.
- `migrations/0011_deletion.sql`: the two functions (added to the isolation suite's closed
  list), `system_access` on `documents` for `app_system`, and `DELETE` on `usage_events` /
  `tenant_usage_counters` for the purge.
