# 006 — Outbox Messaging: Design

- **Spec ID:** 006 `OBX` · **Milestone:** 2
- **Shared design:** [system-design.md §4](../../docs/system-design.md#4-jobs-and-messaging)

## 1. Context and goals

Reliable event publication without distributed transactions. The outbox row is the contract
between write path and any future integrations (webhooks, search indexing elsewhere, audit).

## 2. Scope

**In:** outbox table, `Emit` helper, relay binary, `Publisher` interface + Log impl,
idempotent-consumer pattern (demonstrated by `013`). **Out:** broker cluster ops; consumer
frameworks.

## 3. Components

- `internal/messaging`: `Emit(ctx, tx, tenantID, payload)` with typed payload structs (each names its
  topic), `Publisher` interface, `LogPublisher`, `Permanent` errors, `Relay`, and `Consume` for
  idempotent consumers.
- `cmd/outbox-relay`: poll loop (batch, ordered by id), publish, mark; single active instance,
  enforced by a session advisory lock (`pg_try_advisory_lock`) held on the one connection the relay
  does all its work on (losing the connection releases the lock; a second instance waits on standby).
  `outbox` is under forced RLS and the relay serves every tenant, so it reads and acks only
  through two system functions (`TEN-REQ-010`): `outbox_next(limit)` (oldest unpublished rows)
  and `outbox_mark_published(id)` (guarded `UPDATE ... SET published_at = now() WHERE id = $1
  AND published_at IS NULL RETURNING`), plus `outbox_mark_failed(id, error, max_attempts)` for
  poison rows (§7). Both are owned by `app_system`, which reads `outbox`
  via a `system_access` policy scoped `TO app_system` (no `BYPASSRLS`).
- Publisher implementations: `LogPublisher` first; `PubSubPublisher` (Google Cloud Pub/Sub,
  ordering key = `tenant_id` for per-tenant ordering, OBX-AC-005) when a broker is justified.

## 4. Data model changes

`outbox` (`migrations/0007_outbox.sql`): `id bigserial PK, tenant_id uuid NOT NULL → tenants,
topic text NOT NULL, payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
published_at timestamptz, attempts int, last_error text, quarantined_at timestamptz`.
Partial index `(id) WHERE published_at IS NULL AND quarantined_at IS NULL`. `app` may only
`SELECT`/`INSERT` (RLS); acks go through the system functions.

`consumed_events (consumer, event_id) PK, tenant_id` (RLS): `Consume` inserts here in the
consumer's own tenant transaction, next to its side effect, so a redelivery is a no-op.

## 5. API contract (internal)

```go
type Publisher interface{ Publish(ctx context.Context, ev Event) error }
type Event struct {
    ID int64; Topic string; TenantID uuid.UUID; OccurredAt time.Time; Data json.RawMessage
}
```

Wire format (`Event.MarshalJSON`): `{"event_id", "version": 1, "topic", "tenant_id",
"occurred_at", "data": {...ids...}}`. `document.failed` is emitted only when ingestion gives up
(a permanent failure or the job's last attempt) and carries the reason code, not the raw error.

## 6. Key flows

**Emit:** inside the same `WithTenant` tx as the state change, insert outbox row → commit.
**Relay:** `outbox_next(100)` → for each: publish → `outbox_mark_published(id)`. Crash before mark ⇒ row stays unpublished ⇒ redelivery (OBX-AC-002).

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Publisher error | error | leave unpublished, retry next tick (at-least-once) |
| Relay crash | — | same as above on restart |
| Poison row | publisher returns `Permanent(err)` | `attempts` counter in the row; at `OUTBOX_MAX_ATTEMPTS` (5) the row is quarantined, logged at error, and `Relay.Quarantined` fires (alert hook for `010`) |
| Ordering under failure | — | a failed row holds back its tenant's later rows in the batch (OBX-AC-005); a quarantined row stops holding them |
| Broker outage | transient errors | not counted toward quarantine; poll interval backs off ×2 per failing batch up to 1 min |

## 8. Security and multi-tenancy

Payloads carry ids only — no document text/secrets (consistent with logging rule). `tenant_id`
on every event enables tenant-aware consumers and partitioned ordering (OBX-AC-005).

## 9. Testing strategy

- Integration: emit-in-tx commit/rollback (OBX-AC-001); crash-after-publish simulation → redelivery
  (OBX-AC-002); duplicate consumer idempotency demo (OBX-AC-003); LogPublisher mode (OBX-AC-004);
  per-tenant ordering under concurrency (OBX-AC-005).
- Load: relay lag p95 target (OBX-NFR-001).

## 10. Open questions

- JSON schema per topic → **decided:** typed Go structs, envelope `version` (`SchemaVersion = 1`).
- Ordering is by outbox id among committed rows. Events about one document are causally ordered
  because every emitting transaction first locks the document row; two concurrent transactions
  for different documents of one tenant may commit out of id order, and the later-committing
  lower id is then published after the higher one (never lost).
