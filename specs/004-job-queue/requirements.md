# 004 — Job Queue (PostgreSQL): Requirements

- **Spec ID:** 004 `JOB`
- **Status:** Draft
- **Milestone:** 2 — Async ingestion
- **Depends on:** `specs/001-tenancy-identity`
- **Shared context:** `docs/system-design.md §4` (jobs and messaging)

## Overview

A durable job queue on PostgreSQL: transactional enqueue, `FOR UPDATE SKIP LOCKED` claiming,
leases with heartbeats, bounded retries with exponential backoff + jitter, dead-lettering, and
crash recovery via lease expiry. No broker infrastructure required.

## User stories

- **JOB-US-1:** As a developer, I want to enqueue work in the same transaction as state changes so jobs and data never diverge.
- **JOB-US-2:** As a platform operator, I want crashed workers' jobs to be retried automatically so nothing gets stuck silently.
- **JOB-US-3:** As a platform operator, I want permanently failing jobs marked `dead` and surfaced so I can inspect root causes.

## Requirements

### Functional

- **JOB-REQ-001:** `Queue.Enqueue(ctx, tx, job)` inserts within the caller's transaction: `kind`, `payload` (jsonb), `tenant_id`, optional `idempotency_key`, `run_at` (default now).
- **JOB-REQ-002:** `Claim(ctx, workerID)` atomically claims one runnable job using the claim SQL (`status='queued' AND run_at<=now()` OR expired lease), sets `running`, 60s lease, `attempts+1`.
- **JOB-REQ-003:** Two workers claiming concurrently each get distinct jobs (never the same job twice simultaneously).
- **JOB-REQ-004:** `Heartbeat(ctx, jobID, workerID)` extends the lease while work is in progress.
- **JOB-REQ-005:** `Complete` sets `succeeded`; `Fail` either schedules a retry (`queued`, `run_at = now() + backoff`) or marks `dead` when `attempts >= max_attempts`.
- **JOB-REQ-006:** Backoff = `base * 2^attempts + uniform jitter` (base 5s, cap 10min).
- **JOB-REQ-007:** `idempotency_key` unique per tenant: enqueuing a duplicate is a no-op returning the existing job.
- **JOB-REQ-008:** Recovery sweep: expired leases make jobs claimable again (covered by claim predicate) — a crashed worker's job is retried by another worker.
- **JOB-REQ-009:** Worker core loop: bounded semaphore; reserve capacity **before** claiming; poll interval with jitter; graceful shutdown finishes in-flight jobs.
- **JOB-REQ-010:** Jobs carry `tenant_id`; processing runs inside the tenant context wrapper (`TEN-REQ-009`).

### Non-functional

- **JOB-NFR-001:** Claim latency p95 < 50 ms at 1k queued jobs.
- **JOB-NFR-002:** Throughput ≥ 50 jobs/s on a single Postgres instance in load tests.

## Acceptance criteria (EARS)

- **JOB-AC-001:** WHEN a job is enqueued in a transaction that commits, THE SYSTEM SHALL expose it to claimers only after commit.
- **JOB-AC-002:** WHEN a transaction with an enqueue rolls back, THE SYSTEM SHALL not expose the job.
- **JOB-AC-003:** WHEN two workers claim concurrently, THE SYSTEM SHALL assign each a different job (SKIP LOCKED proof test).
- **JOB-AC-004:** WHEN a worker holding a lease terminates, THE SYSTEM SHALL make the job claimable again after lease expiry with `attempts` already incremented.
- **JOB-AC-005:** WHEN a job fails with `attempts < max_attempts`, THE SYSTEM SHALL requeue it with `run_at` in the future per the backoff formula (± jitter bounds).
- **JOB-AC-006:** WHEN `attempts >= max_attempts`, THE SYSTEM SHALL mark the job `dead` with `last_error` recorded.
- **JOB-AC-007:** WHEN `Enqueue` is called twice with the same `(tenant_id, idempotency_key)`, THE SYSTEM SHALL create one job and return the same id both times.
- **JOB-AC-008:** WHEN a heartbeat arrives within the lease window, THE SYSTEM SHALL extend `lease_expires_at` and the job SHALL NOT be reclaimed while the worker lives.
- **JOB-AC-009:** WHEN shutdown is requested, THE SYSTEM SHALL stop claiming and finish in-flight jobs before exit.

## Out of scope

- Broker integration (Kafka/NATS) — handled by `006` outbox pattern when justified.
- Priority queues, cron/scheduled jobs beyond `run_at` delay.
