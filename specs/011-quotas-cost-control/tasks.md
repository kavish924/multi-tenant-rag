# 011 — Quotas & Cost Control: Tasks

## Group 1: Schema and limiter

- [ ] 1.1 `tenant_quotas` + `tenant_usage_counters` tables. (QTA-REQ-001/008)
- [ ] 1.2 `Limiter.Check`/`Reserve`/`Reconcile` with window UPSERTs + in-process cache. (QTA-REQ-002/003, QTA-NFR-001/002)

## Group 2: Enforcement points

- [ ] 2.1 Ask middleware: questions/day + token projection + concurrency slot → 429 codes before providers. (QTA-REQ-002/004, QTA-AC-001/003/004)
- [ ] 2.2 Upload gate: uploads/day → 429 pre-enqueue. (QTA-AC-002)
- [ ] 2.3 Worker per-tenant concurrency gate. (QTA-REQ-001/002)
- [ ] 2.4 Reconciliation from `usage_events` at stream/job end. (QTA-REQ-003, QTA-AC-008)

## Group 3: Breakers, usage API, cost

- [ ] 3.1 `internal/breaker` state machine wired per provider; 503 fail-fast path. (QTA-REQ-005, QTA-AC-006)
- [ ] 3.2 `GET /tenants/{id}/usage` + price-config cost estimation → metric + endpoint. (QTA-REQ-006/007)
- [ ] 3.3 Operator override path + hot-apply test. (QTA-REQ-008, QTA-AC-007)

## Group 4: Tests and verification

- [ ] 4.1 Race test: 100 concurrent asks at limit → tolerance respected. (QTA-AC-005)
- [ ] 4.2 Breaker transition tests (closed→open→half-open→closed).
- [ ] 4.3 Latency benchmark of check path. (QTA-NFR-001)
