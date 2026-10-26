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
  claim SQL embedded as `claim.sql`, `Backoff(attempts)` helper.
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

**Claim (core SQL):**

```sql
UPDATE jobs SET status='running', locked_by=$1,
       lease_expires_at = now() + interval '60 seconds', attempts = attempts + 1
WHERE id = (
  SELECT id FROM jobs
  WHERE (status='queued' AND run_at <= now())
     OR (status='running' AND lease_expires_at < now())
  ORDER BY run_at
  FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING id, tenant_id, kind, payload, attempts, max_attempts;
```

**Worker loop:** `sem <- struct{}{}` → `Claim` → empty ⇒ release + jittered sleep → else
`go process` (heartbeat ticker + stage runner) → on success `Complete`, on error `Fail`.

## 7. Error handling and failure modes

| Failure | Detection | Response |
| ------- | --------- | -------- |
| Worker crash | lease expiry in claim predicate | job reclaimed, attempts already +1 (JOB-AC-004) |
| Stage error | handler returns error | `Fail` → retry with backoff or `dead` + `last_error` (JOB-AC-005/006) |
| Queue empty | Claim returns nil,nil | release slot, jittered sleep |
| Heartbeat after reclaim | worker id mismatch | stop processing (job was taken over) |

## 8. Security and multi-tenancy

`tenant_id` on every job; consumers process inside `WithTenant`. Job payload never contains
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
