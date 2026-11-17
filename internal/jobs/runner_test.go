package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestBackoffBounds(t *testing.T) {
	zero := func() float64 { return 0 }
	almostOne := func() float64 { return 0.999999 }
	for attempts := 0; attempts <= 20; attempts++ {
		lo, hi := Backoff(attempts, zero), Backoff(attempts, almostOne)
		want := MaxBackoff
		if attempts < 7 {
			want = BackoffBase << attempts
		}
		if lo != min(want, MaxBackoff) {
			t.Errorf("attempts %d: no-jitter backoff %v, want %v", attempts, lo, want)
		}
		if hi < lo || hi > MaxBackoff || hi-lo >= BackoffBase {
			t.Errorf("attempts %d: jitter range [%v, %v] out of bounds", attempts, lo, hi)
		}
	}
	if Backoff(-3, zero) != BackoffBase {
		t.Error("negative attempts not clamped")
	}
}

func TestPermanent(t *testing.T) {
	base := errors.New("corrupt file")
	err := Permanent(base)
	if !IsPermanent(err) || !errors.Is(err, base) {
		t.Fatal("Permanent must be detectable and unwrap to the cause")
	}
	if IsPermanent(base) || Permanent(nil) != nil {
		t.Fatal("plain error reported permanent, or Permanent(nil) != nil")
	}
}

// fakeQueue hands out a fixed list of jobs and records how they settle.
type fakeQueue struct {
	mu        sync.Mutex
	pending   []Job
	completed []uuid.UUID
	failed    map[uuid.UUID]error
	leaseLost map[uuid.UUID]bool // heartbeat returns ErrLeaseLost
	claims    atomic.Int32
}

func newFakeQueue(n int) *fakeQueue {
	q := &fakeQueue{failed: map[uuid.UUID]error{}, leaseLost: map[uuid.UUID]bool{}}
	for i := 0; i < n; i++ {
		q.pending = append(q.pending, Job{ID: uuid.New(), TenantID: uuid.New(), Kind: "k", Attempts: 1, MaxAttempts: 5})
	}
	return q
}

func (q *fakeQueue) Claim(_ context.Context, _ string, kinds []string) (*Job, error) {
	q.claims.Add(1)
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(kinds) == 0 || len(q.pending) == 0 {
		return nil, nil
	}
	j := q.pending[0]
	q.pending = q.pending[1:]
	return &j, nil
}

func (q *fakeQueue) Heartbeat(_ context.Context, j Job, _ string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.leaseLost[j.ID] {
		return ErrLeaseLost
	}
	return nil
}

func (q *fakeQueue) Complete(_ context.Context, j Job, _ string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.completed = append(q.completed, j.ID)
	return nil
}

func (q *fakeQueue) Fail(_ context.Context, j Job, _ string, cause error) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failed[j.ID] = cause
	return IsPermanent(cause), nil
}

func (q *fakeQueue) settled() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.completed) + len(q.failed)
}

func noTenant(uuid.UUID) TenantTx {
	return func(context.Context, func(pgx.Tx) error) error { return nil }
}

func newRunner(q Claimer, h Handler) *Runner {
	return &Runner{
		Queue:         q,
		Handlers:      map[string]Handler{"k": h},
		Tenant:        noTenant,
		Concurrency:   3,
		PollInterval:  5 * time.Millisecond,
		Lease:         30 * time.Millisecond,
		ShutdownGrace: 2 * time.Second,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestRunnerBoundsConcurrencyAndSettles(t *testing.T) {
	q := newFakeQueue(20)
	var running, peak atomic.Int32
	boom := errors.New("boom")
	var calls atomic.Int32
	r := newRunner(q, func(context.Context, Job, TenantTx) error {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		running.Add(-1)
		if calls.Add(1)%4 == 0 {
			return boom
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	waitFor(t, func() bool { return q.settled() == 20 })
	cancel()
	<-done

	if p := peak.Load(); p > 3 {
		t.Errorf("peak concurrency %d exceeds 3", p)
	}
	if len(q.completed) != 15 || len(q.failed) != 5 {
		t.Errorf("completed %d failed %d, want 15/5", len(q.completed), len(q.failed))
	}
}

func TestRunnerPanicAndPermanent(t *testing.T) {
	q := newFakeQueue(2)
	first := q.pending[0].ID
	r := newRunner(q, func(_ context.Context, j Job, _ TenantTx) error {
		if j.ID == first {
			panic("parser exploded")
		}
		return Permanent(errors.New("unsupported file"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	waitFor(t, func() bool { return q.settled() == 2 })
	cancel()
	<-done
	for id, err := range q.failed {
		if id == first && (err == nil || IsPermanent(err)) {
			t.Errorf("panic should be a retryable failure, got %v", err)
		}
		if id != first && !IsPermanent(err) {
			t.Errorf("permanent error lost: %v", err)
		}
	}
}

// JOB-AC-009: shutdown stops claiming and finishes in-flight work.
func TestRunnerGracefulShutdown(t *testing.T) {
	q := newFakeQueue(1)
	started := make(chan struct{})
	r := newRunner(q, func(ctx context.Context, _ Job, _ TenantTx) error {
		close(started)
		select {
		case <-time.After(100 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	<-started
	cancel()
	<-done
	if len(q.completed) != 1 {
		t.Fatalf("in-flight job not finished on shutdown: completed=%v failed=%v", q.completed, q.failed)
	}
	claims := q.claims.Load()
	time.Sleep(20 * time.Millisecond)
	if q.claims.Load() != claims {
		t.Error("runner kept claiming after shutdown")
	}
}

// A worker that loses its lease must stop and must not settle the job.
func TestRunnerLeaseLost(t *testing.T) {
	q := newFakeQueue(1)
	q.leaseLost[q.pending[0].ID] = true
	cancelled := make(chan struct{})
	r := newRunner(q, func(ctx context.Context, _ Job, _ TenantTx) error {
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler not cancelled after lease loss")
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if q.settled() != 0 {
		t.Errorf("job settled after lease loss: completed=%v failed=%v", q.completed, q.failed)
	}
}

// Shutdown grace expiry cancels stuck handlers and leaves the job unsettled
// for lease-expiry recovery.
func TestRunnerGraceExpiry(t *testing.T) {
	q := newFakeQueue(1)
	started := make(chan struct{})
	r := newRunner(q, func(ctx context.Context, _ Job, _ TenantTx) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	r.ShutdownGrace = 20 * time.Millisecond
	r.Lease = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not stop after grace expiry")
	}
	if q.settled() != 0 {
		t.Error("interrupted job must not be settled")
	}
}
