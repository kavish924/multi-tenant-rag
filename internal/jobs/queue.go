// Package jobs is the PostgreSQL job queue (spec 004): transactional
// enqueue, claim through the claim_job system function, leases with
// heartbeats, bounded retries with backoff, and dead-lettering.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/storage"
)

// DefaultLease is how long a claim is owned before another worker may take
// it over. Heartbeats extend it (JOB-REQ-002, JOB-REQ-004).
const DefaultLease = 60 * time.Second

// DefaultMaxAttempts matches the column default.
const DefaultMaxAttempts = 5

// maxErrorLen bounds last_error so a huge error message cannot bloat rows.
const maxErrorLen = 2000

// ErrLeaseLost means the worker no longer owns the job: its lease expired and
// another worker claimed it, or the job is no longer running.
var ErrLeaseLost = errors.New("jobs: lease lost")

// Job is a claimed job. Payload carries ids only, never document text.
type Job struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	Kind        string
	Payload     json.RawMessage
	Attempts    int
	MaxAttempts int
}

// EnqueueParams describes a new job.
type EnqueueParams struct {
	TenantID uuid.UUID
	Kind     string
	Payload  any // marshaled to JSON; ids only
	// IdempotencyKey, when set, makes Enqueue a no-op for an existing
	// (tenant_id, idempotency_key) and returns that job's id (JOB-REQ-007).
	IdempotencyKey string
	RunAt          time.Time // zero = now
	MaxAttempts    int       // zero = DefaultMaxAttempts
}

// Enqueue inserts a job inside the caller's transaction, which must be a
// WithTenant transaction for p.TenantID: the job becomes visible to
// claimers only when that transaction commits (JOB-AC-001/002). created is
// false when an existing job with the same idempotency key was returned.
func Enqueue(ctx context.Context, tx pgx.Tx, p EnqueueParams) (id uuid.UUID, created bool, err error) {
	if p.TenantID == uuid.Nil || p.Kind == "" {
		return uuid.Nil, false, errors.New("jobs: tenant id and kind are required")
	}
	payload := []byte("{}")
	if p.Payload != nil {
		if payload, err = json.Marshal(p.Payload); err != nil {
			return uuid.Nil, false, fmt.Errorf("jobs: payload: %w", err)
		}
	}
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	var runAt any // NULL → column default now()
	if !p.RunAt.IsZero() {
		runAt = p.RunAt
	}
	var key any
	if p.IdempotencyKey != "" {
		key = p.IdempotencyKey
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO jobs (tenant_id, kind, payload, idempotency_key, run_at, max_attempts)
		VALUES ($1, $2, $3, $4, coalesce($5::timestamptz, now()), $6)
		ON CONFLICT (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING id`,
		p.TenantID, p.Kind, payload, key, runAt, maxAttempts).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) || key == nil {
		return uuid.Nil, false, fmt.Errorf("jobs: enqueue: %w", err)
	}
	err = tx.QueryRow(ctx,
		`SELECT id FROM jobs WHERE tenant_id = $1 AND idempotency_key = $2`,
		p.TenantID, p.IdempotencyKey).Scan(&id)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("jobs: enqueue existing: %w", err)
	}
	return id, false, nil
}

// Queue claims and settles jobs. Claim crosses tenants through claim_job;
// Heartbeat, Complete and Fail run inside WithTenant(job.TenantID) and are
// guarded by locked_by, so a worker whose lease was taken over cannot write.
type Queue struct {
	store *storage.Store
	lease time.Duration
}

// NewQueue builds a queue; lease <= 0 uses DefaultLease.
func NewQueue(store *storage.Store, lease time.Duration) *Queue {
	if lease <= 0 {
		lease = DefaultLease
	}
	return &Queue{store: store, lease: lease}
}

// Lease returns the claim lease duration.
func (q *Queue) Lease() time.Duration { return q.lease }

// Claim takes one runnable job of the given kinds, or returns nil, nil when
// none is runnable.
func (q *Queue) Claim(ctx context.Context, workerID string, kinds []string) (*Job, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	var j Job
	err := q.store.Pool().QueryRow(ctx,
		`SELECT id, tenant_id, kind, payload, attempts, max_attempts FROM claim_job($1, $2, $3)`,
		workerID, q.lease, kinds).
		Scan(&j.ID, &j.TenantID, &j.Kind, &j.Payload, &j.Attempts, &j.MaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: claim: %w", err)
	}
	return &j, nil
}

// Heartbeat extends the lease while the worker still owns the job.
func (q *Queue) Heartbeat(ctx context.Context, job Job, workerID string) error {
	return q.settle(ctx, job, `
		UPDATE jobs SET lease_expires_at = now() + $3::interval, updated_at = now()
		WHERE id = $1 AND locked_by = $2 AND status = 'running'`,
		job.ID, workerID, q.lease)
}

// Complete marks the job succeeded.
func (q *Queue) Complete(ctx context.Context, job Job, workerID string) error {
	return q.settle(ctx, job, `
		UPDATE jobs SET status = 'succeeded', locked_by = NULL, lease_expires_at = NULL,
		       last_error = NULL, updated_at = now()
		WHERE id = $1 AND locked_by = $2 AND status = 'running'`,
		job.ID, workerID)
}

// Fail records cause and either schedules a retry after Backoff(attempts) or
// marks the job dead when attempts are exhausted or cause is Permanent
// (JOB-REQ-005/006). dead reports which happened.
func (q *Queue) Fail(ctx context.Context, job Job, workerID string, cause error) (dead bool, err error) {
	msg := "unknown error"
	if cause != nil {
		msg = cause.Error()
	}
	if len(msg) > maxErrorLen {
		msg = msg[:maxErrorLen]
	}
	permanent := IsPermanent(cause)
	delay := Backoff(job.Attempts, nil)

	var status string
	err = q.store.WithTenant(ctx, job.TenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			UPDATE jobs SET
			  status = CASE WHEN $3 OR attempts >= max_attempts THEN 'dead' ELSE 'queued' END,
			  run_at = CASE WHEN $3 OR attempts >= max_attempts THEN run_at ELSE now() + $4::interval END,
			  locked_by = NULL, lease_expires_at = NULL, last_error = $5, updated_at = now()
			WHERE id = $1 AND locked_by = $2 AND status = 'running'
			RETURNING status`,
			job.ID, workerID, permanent, delay, msg).Scan(&status)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrLeaseLost
	}
	if err != nil {
		return false, fmt.Errorf("jobs: fail: %w", err)
	}
	return status == "dead", nil
}

func (q *Queue) settle(ctx context.Context, job Job, sql string, args ...any) error {
	var n int64
	err := q.store.WithTenant(ctx, job.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, args...)
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

// Permanent marks an error as not worth retrying: Fail dead-letters the job
// immediately (for example an unsupported or corrupt file).
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether err was wrapped with Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}
