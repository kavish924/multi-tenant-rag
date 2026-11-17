# 006 — Outbox Messaging: Tasks

## Group 1: Table and emit

- [x] 1.1 `outbox` table + partial unpublished index. (OBX-REQ-001)
- [x] 1.2 `Outbox.Emit(ctx, tx, topic, tenantID, payload)` helper + typed event structs for the four topics. (OBX-REQ-001/002/005)

## Group 2: Relay

- [x] 2.1 `Publisher` interface + `LogPublisher`. (OBX-REQ-004, OBX-AC-004)
- [x] 2.2 `cmd/outbox-relay`: batched poll, ordered publish, guarded mark-published. (OBX-REQ-003, OBX-AC-002/005)
- [x] 2.3 Poison-row quarantine after max publish attempts (alert hook for `010`). (design §7)

## Group 3: Producer wiring

- [x] 3.1 Emit `document.created` (upload tx, `002`), `document.indexed` / `document.failed` (pipeline, `005`), `document.deleting` (delete, `002`/`013`). (OBX-REQ-002, ING-AC-008)

## Group 4: Tests and verification

- [ ] 4.1 *(written in `internal/messaging/outbox_integration_test.go`, not yet run; relay logic also unit-tested)* Commit/rollback atomicity tests. (OBX-AC-001)
- [ ] 4.2 *(written in `internal/messaging/outbox_integration_test.go`, not yet run; relay logic also unit-tested)* Redelivery test: publish-without-mark crash → duplicate delivery → idempotent consumer demo. (OBX-AC-002/003)
- [ ] 4.3 *(written in `internal/messaging/outbox_integration_test.go`, not yet run; relay logic also unit-tested)* Ordering test under concurrent emits. (OBX-AC-005)
- [ ] 4.4 *(smoke check in `TestRelayRun`; benchmark pending a load environment)* Lag benchmark vs OBX-NFR-001.
