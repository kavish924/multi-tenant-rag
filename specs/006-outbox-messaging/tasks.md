# 006 — Outbox Messaging: Tasks

## Group 1: Table and emit

- [ ] 1.1 `outbox` table + partial unpublished index. (OBX-REQ-001)
- [ ] 1.2 `Outbox.Emit(ctx, tx, topic, tenantID, payload)` helper + typed event structs for the four topics. (OBX-REQ-001/002/005)

## Group 2: Relay

- [ ] 2.1 `Publisher` interface + `LogPublisher`. (OBX-REQ-004, OBX-AC-004)
- [ ] 2.2 `cmd/outbox-relay`: batched poll, ordered publish, guarded mark-published. (OBX-REQ-003, OBX-AC-002/005)
- [ ] 2.3 Poison-row quarantine after max publish attempts (alert hook for `010`). (design §7)

## Group 3: Producer wiring

- [ ] 3.1 Emit `document.created` (upload tx, `002`), `document.indexed` / `document.failed` (pipeline, `005`), `document.deleting` (delete, `002`/`013`). (OBX-REQ-002, ING-AC-008)

## Group 4: Tests and verification

- [ ] 4.1 Commit/rollback atomicity tests. (OBX-AC-001)
- [ ] 4.2 Redelivery test: publish-without-mark crash → duplicate delivery → idempotent consumer demo. (OBX-AC-002/003)
- [ ] 4.3 Ordering test under concurrent emits. (OBX-AC-005)
- [ ] 4.4 Lag benchmark vs OBX-NFR-001.
