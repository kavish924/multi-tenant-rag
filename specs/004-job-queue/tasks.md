# 004 — Job Queue: Tasks

## Group 1: Schema

- [x] 1.1 `jobs` table + CHECK + partial unique `(tenant_id, idempotency_key)` + `(status, run_at)` index. (JOB-REQ-001, JOB-REQ-007)

## Group 2: Queue core

- [x] 2.1 `Queue.Enqueue` (tx-scoped, idempotency no-op returning existing id). (JOB-REQ-001, JOB-REQ-007, JOB-AC-001/002/007)
- [x] 2.2 `migrations/0004_claim_job.sql` (`claim_job` system function: reap-then-claim, owned by `app_system`, EXECUTE to `app`) + `Claim` calling it. (JOB-REQ-002, JOB-REQ-008, JOB-REQ-011, JOB-AC-003/004/010/011)
- [x] 2.3 `Heartbeat`, `Complete`, `Fail` (retry-vs-dead logic). (JOB-REQ-004/005, JOB-AC-006/008)
- [x] 2.4 `Backoff(attempts)` = 5s · 2^n + jitter, cap 10 min; unit tests on bounds. (JOB-REQ-006, JOB-AC-005)

## Group 3: Worker runtime

- [x] 3.1 Bounded-pool loop: semaphore reserved before claim; jittered poll. (JOB-REQ-009)
- [x] 3.2 Graceful shutdown on SIGTERM (sent by Cloud Run on scale-in/redeploy): stop claiming, `WaitGroup.Wait` within the shutdown grace period. (JOB-REQ-009, JOB-AC-009)
- [x] 3.3 Heartbeat ticker inside `process`; abandon processing if worker no longer owns the job (lease takeover). (JOB-REQ-004)
- [x] 3.4 Tenant context: wrap stage execution in `WithTenant(job.TenantID)`. (JOB-REQ-010)

## Group 4: Tests and verification

- [ ] 4.1 *(written in `internal/jobs/queue_integration_test.go`, not yet run)* Concurrency test: 8 workers × race claim → no double-claim (JOB-AC-003).
- [ ] 4.2 Crash test: SIGKILL worker → job reclaimed after lease (JOB-AC-004).
- [ ] 4.3 *(written in `internal/jobs/queue_integration_test.go`, not yet run)* Retry ladder test: failures 1..max → backoff schedule then `dead` + `last_error` (JOB-AC-005/006).
- [ ] 4.3a *(written in `internal/jobs/queue_integration_test.go`, not yet run)* Poison-pill test: SIGKILL the worker on every attempt → after `max_attempts` the job is `dead` and never claimed again (JOB-AC-010).
- [ ] 4.3b *(written in `internal/jobs/queue_integration_test.go`, not yet run)* RLS test: worker role sees zero rows via plain `SELECT FROM jobs` yet `claim_job` returns jobs from two different tenants (JOB-AC-011).
- [ ] 4.4 Load test: ≥ 50 jobs/s sustained; claim p95 < 50 ms (JOB-NFR-001/002).
- [ ] 4.5 **Milestone 2 exit contribution:** restart/kill worker → no corrupted document state (with `005`).
