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

- `internal/messaging`: `Outbox.Emit`, `Publisher` interface, `LogPublisher`.
- `cmd/outbox-relay`: poll loop (batch, ordered by id), publish, mark; single active instance
  (row-level claim via `UPDATE ... SET published_at = now() WHERE id = $1 AND published_at IS NULL RETURNING`).

## 4. Data model changes

`outbox` (in `0001_init.sql`): `id bigserial PK, tenant_id uuid NOT NULL, topic text NOT NULL,
payload jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), published_at timestamptz`.
Partial index `(published_at) WHERE published_at IS NULL`.

## 5. API contract (internal)

```go
type Publisher interface{ Publish(ctx context.Context, ev Event) error }
type Event struct {
    ID int64; Topic string; TenantID uuid.UUID; OccurredAt time.Time; Data map[string]any
}
```

## 6. Key flows

**Emit:** inside the same `WithTenant` tx as the state change, insert outbox row → commit.
**Relay:** select oldest unpublished batch (e.g. 100) → for each: publish → mark (guarded
update). Crash before mark ⇒ row stays unpublished ⇒ redelivery (OBX-AC-002).

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Publisher error | error | leave unpublished, retry next tick (at-least-once) |
| Relay crash | — | same as above on restart |
| Poison row | repeated publish failure | max attempts counter → quarantine log + alert (`010` metric) |

## 8. Security and multi-tenancy

Payloads carry ids only — no document text/secrets (consistent with logging rule). `tenant_id`
on every event enables tenant-aware consumers and partitioned ordering (OBX-AC-005).

## 9. Testing strategy

- Integration: emit-in-tx commit/rollback (OBX-AC-001); crash-after-publish simulation → redelivery
  (OBX-AC-002); duplicate consumer idempotency demo (OBX-AC-003); LogPublisher mode (OBX-AC-004);
  per-tenant ordering under concurrency (OBX-AC-005).
- Load: relay lag p95 target (OBX-NFR-001).

## 10. Open questions

- JSON schema per topic (typed Go structs serialized) → **default:** typed structs, versioned field names.
