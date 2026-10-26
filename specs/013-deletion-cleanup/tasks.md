# 013 — Deletion & Cleanup: Tasks

## Group 1: Cleanup job

- [ ] 1.1 `cleanup` job kind: chunks delete + invariant assert + object delete + status `deleted`. (DEL-REQ-002/003/004, DEL-AC-002)
- [ ] 1.2 Idempotency: `cleanup:{document_id}` key; duplicate-event no-op consumer (reference impl for `006`). (DEL-REQ-006, DEL-AC-005)
- [ ] 1.3 Retry ladder integration for partial failures. (DEL-AC-004)

## Group 2: Purge and sweeper

- [ ] 2.1 `POST /tenants/{id}/purge` (owner) → fan-out cleanup per document + usage-row purge. (DEL-REQ-005, DEL-AC-006)
- [ ] 2.2 Retention sweeper: expired metadata removal + orphan-object report. (DEL-REQ-007, DEL-AC-008)

## Group 3: Race safety

- [ ] 3.1 Interleaving tests: delete×embed, delete×index (scheduled via instrumented pipeline). (DEL-AC-003)
- [ ] 3.2 Immediate-exclusion timing test (≤ 1 s). (DEL-NFR-001, DEL-AC-001)

## Group 4: Tests and verification

- [ ] 4.1 Authz matrix tests for delete/purge. (DEL-AC-007)
- [ ] 4.2 Performance: 5k-chunk cleanup < 60 s. (DEL-NFR-002)
- [ ] 4.3 **Red-team contribution:** deletion-races pass condition green ("deleted content never reappears").
