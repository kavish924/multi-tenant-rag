//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/jobs"
	"github.com/kavish924/multi-tenant-rag/internal/testutil/pgtest"
)

var env *pgtest.Env

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	e, err := pgtest.Start(ctx)
	cancel()
	if err != nil {
		log.Fatalf("pgtest: %v", err)
	}
	env = e
	code := m.Run()
	env.Close()
	os.Exit(code)
}

var kinds = []string{"ingest"}

func tenant(t *testing.T) uuid.UUID {
	t.Helper()
	return env.SeedTenant(t, "t-"+uuid.NewString()[:8], nil)
}

func enqueue(t *testing.T, tenantID uuid.UUID, p jobs.EnqueueParams) uuid.UUID {
	t.Helper()
	p.TenantID = tenantID
	if p.Kind == "" {
		p.Kind = "ingest"
	}
	var id uuid.UUID
	err := env.Store.WithTenant(context.Background(), tenantID, func(tx pgx.Tx) error {
		var err error
		id, _, err = jobs.Enqueue(context.Background(), tx, p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func status(t *testing.T, id uuid.UUID) (st string, attempts int, lastErr *string) {
	t.Helper()
	if err := env.Admin.QueryRow(context.Background(),
		`SELECT status, attempts, last_error FROM jobs WHERE id = $1`, id).Scan(&st, &attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	return
}

func expireLease(t *testing.T, id uuid.UUID) {
	t.Helper()
	if _, err := env.Admin.Exec(context.Background(),
		`UPDATE jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

func makeRunnable(t *testing.T, id uuid.UUID) {
	t.Helper()
	if _, err := env.Admin.Exec(context.Background(), `UPDATE jobs SET run_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// JOB-AC-001/002: visible only after commit; rollback leaves nothing.
func TestEnqueueIsTransactional(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := tenant(t)
	q := jobs.NewQueue(env.Store, 0)

	rollback := errors.New("rollback")
	err := env.Store.WithTenant(ctx, tid, func(tx pgx.Tx) error {
		if _, _, err := jobs.Enqueue(ctx, tx, jobs.EnqueueParams{TenantID: tid, Kind: "ingest"}); err != nil {
			return err
		}
		if j, err := q.Claim(ctx, "w", kinds); err != nil || j != nil {
			t.Errorf("uncommitted job claimable: %v %v", j, err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if j, _ := q.Claim(ctx, "w", kinds); j != nil {
		t.Fatal("rolled-back job claimable")
	}

	id := enqueue(t, tid, jobs.EnqueueParams{})
	j, err := q.Claim(ctx, "w", kinds)
	if err != nil || j == nil || j.ID != id || j.Attempts != 1 || j.TenantID != tid {
		t.Fatalf("claim after commit: %+v %v", j, err)
	}
}

// JOB-AC-003: concurrent claimers never get the same job.
func TestConcurrentClaimsAreDistinct(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := tenant(t)
	q := jobs.NewQueue(env.Store, 0)
	const n = 40
	for i := 0; i < n; i++ {
		enqueue(t, tid, jobs.EnqueueParams{})
	}
	var mu sync.Mutex
	seen := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				j, err := q.Claim(ctx, "w"+string(rune('a'+w)), kinds)
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("claimed %d distinct jobs, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("job %s claimed %d times", id, c)
		}
	}
}

// JOB-AC-004, JOB-AC-008: a crashed worker's job is reclaimed after lease
// expiry with attempts incremented; the old worker can no longer write; a
// live heartbeat prevents reclaim.
func TestLeaseExpiryAndHeartbeat(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := tenant(t)
	q := jobs.NewQueue(env.Store, time.Minute)
	id := enqueue(t, tid, jobs.EnqueueParams{})

	j1, _ := q.Claim(ctx, "w1", kinds)
	if err := q.Heartbeat(ctx, *j1, "w1"); err != nil {
		t.Fatalf("heartbeat by owner: %v", err)
	}
	if j, _ := q.Claim(ctx, "w2", kinds); j != nil {
		t.Fatal("job with live lease reclaimed")
	}

	expireLease(t, id) // w1 "crashed"
	j2, err := q.Claim(ctx, "w2", kinds)
	if err != nil || j2 == nil || j2.ID != id || j2.Attempts != 2 {
		t.Fatalf("reclaim: %+v %v", j2, err)
	}
	if err := q.Heartbeat(ctx, *j1, "w1"); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Errorf("old owner heartbeat: %v, want ErrLeaseLost", err)
	}
	if err := q.Complete(ctx, *j1, "w1"); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Errorf("old owner complete: %v, want ErrLeaseLost", err)
	}
	if err := q.Complete(ctx, *j2, "w2"); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := status(t, id); st != "succeeded" {
		t.Errorf("status %s, want succeeded", st)
	}
}

// JOB-AC-005/006: retries back off, then the job is dead with last_error.
func TestRetryLadderThenDead(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := tenant(t)
	q := jobs.NewQueue(env.Store, 0)
	id := enqueue(t, tid, jobs.EnqueueParams{MaxAttempts: 3})

	for attempt := 1; attempt <= 3; attempt++ {
		j, err := q.Claim(ctx, "w", kinds)
		if err != nil || j == nil || j.Attempts != attempt {
			t.Fatalf("attempt %d claim: %+v %v", attempt, j, err)
		}
		before := time.Now()
		dead, err := q.Fail(ctx, *j, "w", errors.New("embed timeout"))
		if err != nil {
			t.Fatal(err)
		}
		if attempt < 3 {
			if dead {
				t.Fatalf("attempt %d: dead too early", attempt)
			}
			var runAt time.Time
			_ = env.Admin.QueryRow(ctx, `SELECT run_at FROM jobs WHERE id = $1`, id).Scan(&runAt)
			lo := before.Add(jobs.Backoff(attempt, func() float64 { return 0 }) - 2*time.Second)
			hi := before.Add(jobs.Backoff(attempt, func() float64 { return 0.999 }) + 2*time.Second)
			if runAt.Before(lo) || runAt.After(hi) {
				t.Errorf("attempt %d: run_at %v outside [%v, %v]", attempt, runAt, lo, hi)
			}
			if j2, _ := q.Claim(ctx, "w", kinds); j2 != nil {
				t.Fatal("job claimable before its backoff elapsed")
			}
			makeRunnable(t, id)
		} else if !dead {
			t.Fatal("final failure did not dead-letter")
		}
	}
	st, attempts, lastErr := status(t, id)
	if st != "dead" || attempts != 3 || lastErr == nil || *lastErr != "embed timeout" {
		t.Errorf("final state %s attempts=%d last_error=%v", st, attempts, lastErr)
	}
}

func TestPermanentFailureIsDeadImmediately(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := tenant(t)
	q := jobs.NewQueue(env.Store, 0)
	id := enqueue(t, tid, jobs.EnqueueParams{})
	j, _ := q.Claim(ctx, "w", kinds)
	dead, err := q.Fail(ctx, *j, "w", jobs.Permanent(errors.New("not a pdf")))
	if err != nil || !dead {
		t.Fatalf("dead=%v err=%v", dead, err)
	}
	if st, _, _ := status(t, id); st != "dead" {
		t.Errorf("status %s", st)
	}
}

// JOB-AC-007: idempotent enqueue.
func TestIdempotentEnqueue(t *testing.T) {
	env.Truncate(t)
	tid := tenant(t)
	other := tenant(t)
	a := enqueue(t, tid, jobs.EnqueueParams{IdempotencyKey: "ingest:doc-1"})
	b := enqueue(t, tid, jobs.EnqueueParams{IdempotencyKey: "ingest:doc-1"})
	c := enqueue(t, other, jobs.EnqueueParams{IdempotencyKey: "ingest:doc-1"})
	if a != b {
		t.Errorf("same key returned %s and %s", a, b)
	}
	if c == a {
		t.Error("idempotency key leaked across tenants")
	}
	var n int
	_ = env.Admin.QueryRow(context.Background(), `SELECT count(*) FROM jobs`).Scan(&n)
	if n != 2 {
		t.Errorf("%d jobs, want 2", n)
	}
}

// JOB-AC-010: a job whose lease expires on every attempt ends dead and is
// never handed out again.
func TestPoisonPillIsReaped(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := tenant(t)
	q := jobs.NewQueue(env.Store, 0)
	id := enqueue(t, tid, jobs.EnqueueParams{MaxAttempts: 2})

	for attempt := 1; attempt <= 2; attempt++ {
		j, _ := q.Claim(ctx, "w", kinds)
		if j == nil || j.Attempts != attempt {
			t.Fatalf("attempt %d: %+v", attempt, j)
		}
		expireLease(t, id) // worker killed mid-job
	}
	if j, _ := q.Claim(ctx, "w", kinds); j != nil {
		t.Fatalf("poison pill handed out again: %+v", j)
	}
	st, _, lastErr := status(t, id)
	if st != "dead" || lastErr == nil {
		t.Errorf("status %s last_error %v, want dead with reason", st, lastErr)
	}
}

// JOB-AC-011: the worker role sees no jobs with a plain SELECT, yet
// claim_job finds jobs of every tenant.
func TestClaimCrossesTenantsOnlyThroughSystemFunction(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	a, b := tenant(t), tenant(t)
	enqueue(t, a, jobs.EnqueueParams{})
	enqueue(t, b, jobs.EnqueueParams{})

	var visible int
	if err := env.App.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != 0 {
		t.Errorf("app sees %d jobs without tenant context", visible)
	}
	q := jobs.NewQueue(env.Store, 0)
	got := map[uuid.UUID]bool{}
	for i := 0; i < 2; i++ {
		j, err := q.Claim(ctx, "w", kinds)
		if err != nil || j == nil {
			t.Fatalf("claim %d: %v %v", i, j, err)
		}
		got[j.TenantID] = true
	}
	if !got[a] || !got[b] {
		t.Errorf("claimed tenants %v, want both", got)
	}
}

func TestClaimFiltersByKind(t *testing.T) {
	env.Truncate(t)
	ctx := context.Background()
	tid := tenant(t)
	enqueue(t, tid, jobs.EnqueueParams{Kind: "cleanup"})
	q := jobs.NewQueue(env.Store, 0)
	if j, _ := q.Claim(ctx, "w", []string{"ingest"}); j != nil {
		t.Fatal("claimed a kind the worker does not handle")
	}
	if j, _ := q.Claim(ctx, "w", []string{"cleanup"}); j == nil {
		t.Fatal("matching kind not claimed")
	}
}
