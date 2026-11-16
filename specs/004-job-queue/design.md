# 004 — Job Queue (PostgreSQL): Design

- **Spec ID:** 004 `JOB` · **Milestone:** 2
- **Shared design:** [system-design.md §4 Jobs and messaging](../../docs/system-design.md#4-jobs-and-messaging)

## 1. Context and goals

Durable asynchronous work with exactly the semantics the ingestion pipeline needs: atomic
enqueue-with-state, single ownership, crash recovery, bounded retries. Postgres-only by design.

## 2. Scope

**In:** `jobs` schema, `Queue` implementation, claim SQL, heartbeat/backoff/dead-letter,
worker loop. **Out:** pipeline stages (`005`), broker relay (`006`), queue metrics (`010`).

## 3. Components

- `internal/jobs`: `Queue` interface (`Enqueue`, `Claim`, `Heartbeat`, `Complete`, `Fail`),
  `Claim` calling the `claim_job` system function (`migrations/0004_claim_job.sql`), `Backoff(attempts)` helper.
- `cmd/worker`: loop with `sem` semaphore + `WaitGroup` (per JOB-REQ-009/AC-009).

## 4. Data model changes

`jobs` table (in `0001_init.sql`): `id, tenant_id, kind, payload jsonb,
status CHECK IN ('queued','running','succeeded','failed','dead'), attempts, max_attempts
(default 5), run_at, lease_expires_at, locked_by, idempotency_key, last_error`,
`UNIQUE (tenant_id, idempotency_key)` (partial: `WHERE idempotency_key IS NOT NULL`).

Claim-supporting index: `(status, run_at)`.

## 5. API contract (internal)

```go
type Job struct { ID, TenantID uuid.UUID; Kind string; Payload []byte; Attempts, MaxAttempts int }
Enqueue(ctx, tx pgx.Tx, kind string, tenantID uuid.UUID, payload []byte, idemKey string, runAt *time.Time) (uuid.UUID, error)
Claim(ctx, workerID string) (*Job, error)   // nil,nil when queue empty
Heartbeat(ctx, jobID uuid.UUID, workerID string) error
Complete(ctx, jobID uuid.UUID) error
Fail(ctx, jobID uuid.UUID, cause error) error // retry or dead per attempts
```

## 6. Key flows

**Claim:** `jobs` is under forced RLS and the claim must see every tenant's jobs, so it is the
`claim_job(worker_id, lease)` system function (`migrations/0004_claim_job.sql`, full SQL in
[system-design.md §4](../../docs/system-design.md#claiming-a-job)). `Claim` runs
`SELECT * FROM claim_job($1, interval '60 seconds')`. The function, in one statement:

1. **Reaps** expired-lease jobs with `attempts >= max_attempts` → `dead` (JOB-REQ-011).
2. **Claims** one job: `queued AND run_at <= now()` OR expired lease `AND attempts < max_attempts`,
   `ORDER BY run_at FOR UPDATE SKIP LOCKED LIMIT 1` → `running`, lease, `attempts + 1`.

`Heartbeat`/`Complete`/`Fail` run as plain statements inside `WithTenant(job.TenantID)`, each
guarded by `WHERE id = $1 AND locked_by = $worker` so a worker whose lease was taken over cannot
write.

**Worker loop:** `sem <- struct{}{}` → `Claim` → empty ⇒ release + jittered sleep → else
`go process` (heartbeat ticker + stage runner) → on success `Complete`, on error `Fail`.

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Worker crash | lease expiry in claim predicate | job reclaimed, attempts already +1 (JOB-AC-004) |
| Job crashes worker every attempt | expired lease with `attempts >= max_attempts` | reaped to `dead` by `claim_job` (JOB-AC-010) |
| Stage error | handler returns error | `Fail` → retry with backoff or `dead` + `last_error` (JOB-AC-005/006) |
| Queue empty | Claim returns nil,nil | release slot, jittered sleep |
| Heartbeat after reclaim | worker id mismatch | stop processing (job was taken over) |

## 8. Security and multi-tenancy

`tenant_id` on every job; consumers process inside `WithTenant`. The only cross-tenant access is
`claim_job`, which returns ids and the (id-only) payload — never document content. Job payload never contains
document text or secrets — only ids (logging rule in `010`).

## 9. Testing strategy

- Integration (Postgres testcontainer): concurrent-claim race with N workers (JOB-AC-003);
  lease-expiry recovery with clock control (JOB-AC-004); backoff math (JOB-AC-005);
  dead-lettering (JOB-AC-006); idempotent enqueue (JOB-AC-007); heartbeat extension (JOB-AC-008);
  transactional enqueue commit/rollback (JOB-AC-001/002).
- Failure & load: kill -9 worker mid-job → recovery; throughput benchmark (JOB-NFR-002);
  graceful shutdown test (JOB-AC-009).

## 10. Open questions

- Visibility timeout vs run_at ordering fairness → **default:** FIFO by `run_at`; fair enough.
