package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/trace"

	"github.com/kavish924/multi-tenant-rag/internal/observability"
)

// Claimer is the queue surface the runner needs (implemented by *Queue).
type Claimer interface {
	Claim(ctx context.Context, workerID string, kinds []string) (*Job, error)
	Heartbeat(ctx context.Context, job Job, workerID string) error
	Complete(ctx context.Context, job Job, workerID string) error
	Fail(ctx context.Context, job Job, workerID string, cause error) (dead bool, err error)
}

// TenantTx runs fn in a transaction scoped to the job's tenant
// (TEN-REQ-009, JOB-REQ-010). Handlers do all their database work through
// it, in short transactions, never across a slow external call.
type TenantTx func(ctx context.Context, fn func(pgx.Tx) error) error

// TenantScoper binds a tenant id to TenantTx; *storage.Store satisfies it
// through StoreScoper.
type TenantScoper func(tenantID uuid.UUID) TenantTx

// Handler processes one job. Returning nil completes it; an error schedules
// a retry (or dead-letters it when attempts are exhausted or the error is
// Permanent). ctx is cancelled if the lease is lost or shutdown runs out of
// grace time.
type Handler func(ctx context.Context, job Job, tx TenantTx) error

// Runner is the worker core loop (JOB-REQ-009): a bounded pool whose slot
// is reserved before claiming, jittered polling, heartbeats, and graceful
// shutdown that finishes in-flight jobs.
type Runner struct {
	Queue    Claimer
	Handlers map[string]Handler
	Tenant   TenantScoper
	WorkerID string

	Concurrency   int           // default 4
	PollInterval  time.Duration // default 1s (jittered ±50%)
	Lease         time.Duration // heartbeat every Lease/3; default DefaultLease
	ShutdownGrace time.Duration // default 8s (Cloud Run allows 10s after SIGTERM)
	Log           *slog.Logger
}

func (r *Runner) defaults() {
	if r.Concurrency <= 0 {
		r.Concurrency = 4
	}
	if r.PollInterval <= 0 {
		r.PollInterval = time.Second
	}
	if r.Lease <= 0 {
		r.Lease = DefaultLease
	}
	if r.ShutdownGrace <= 0 {
		r.ShutdownGrace = 8 * time.Second
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.WorkerID == "" {
		r.WorkerID = "worker-" + uuid.NewString()
	}
}

// Run claims and processes jobs until ctx is cancelled, then stops claiming
// and waits up to ShutdownGrace for in-flight jobs (JOB-AC-009). Jobs still
// running after that are cancelled; their leases expire and another worker
// retries them.
func (r *Runner) Run(ctx context.Context) error {
	r.defaults()
	if r.Queue == nil || r.Tenant == nil {
		return errors.New("jobs: runner needs Queue and Tenant")
	}
	kinds := make([]string, 0, len(r.Handlers))
	for k := range r.Handlers {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	r.Log.Info("worker started", "worker_id", r.WorkerID, "kinds", kinds, "concurrency", r.Concurrency)

	// In-flight jobs outlive ctx until the grace period ends.
	jobCtx, cancelJobs := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelJobs()

	sem := make(chan struct{}, r.Concurrency)
	var wg sync.WaitGroup

loop:
	for {
		select {
		case sem <- struct{}{}: // reserve capacity before claiming
		case <-ctx.Done():
			break loop
		}
		if ctx.Err() != nil {
			<-sem
			break
		}
		claimStart := time.Now()
		job, err := r.Queue.Claim(ctx, r.WorkerID, kinds)
		claimEnd := time.Now()
		if err != nil || job == nil {
			<-sem
			if err != nil && ctx.Err() == nil {
				r.Log.Error("claim failed", "err", err)
			}
			if !sleep(ctx, jittered(r.PollInterval)) {
				break
			}
			continue
		}
		wg.Add(1)
		observability.M().JobAttempt(ctx, job.Kind)
		go func(j Job) {
			defer wg.Done()
			defer func() { <-sem }()
			r.process(jobCtx, j, claimStart, claimEnd)
		}(*job)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(r.ShutdownGrace):
		r.Log.Warn("shutdown grace expired; cancelling in-flight jobs")
		cancelJobs()
		<-done
	}
	r.Log.Info("worker stopped", "worker_id", r.WorkerID)
	return nil
}

func (r *Runner) process(parent context.Context, job Job, claimStart, claimEnd time.Time) {
	log := r.Log.With("job_id", job.ID, "tenant_id", job.TenantID, "kind", job.Kind, "attempt", job.Attempts)
	// The job span continues the enqueuing request's trace (OBS-AC-002) and
	// starts at the claim, which is recorded as its first child.
	tctx := observability.WithTraceParent(parent, traceParentOf(job.Payload))
	tctx, span := observability.Tracer().Start(tctx, "job "+job.Kind,
		trace.WithSpanKind(trace.SpanKindConsumer), trace.WithTimestamp(claimStart),
		trace.WithAttributes(observability.AttrJobID.String(job.ID.String()), observability.AttrJobKind.String(job.Kind),
			observability.AttrTenantID.String(job.TenantID.String())))
	observability.SpanAt(tctx, "claim", claimStart, claimEnd)
	var jobErr error
	defer func() { observability.End(span, jobErr) }()
	parent = tctx
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var lost atomic.Bool
	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(r.Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				err := r.Queue.Heartbeat(ctx, job, r.WorkerID)
				if errors.Is(err, ErrLeaseLost) {
					log.Warn("lease lost; abandoning job")
					lost.Store(true)
					cancel()
					return
				}
				if err != nil && ctx.Err() == nil {
					log.Error("heartbeat failed", "err", err)
				}
			}
		}
	}()

	err := r.run(ctx, job)
	jobErr = err
	close(hbStop)
	<-hbDone

	if lost.Load() {
		return // another worker owns it now; writing would be wrong
	}
	if parent.Err() != nil {
		log.Warn("job interrupted by shutdown; lease expiry will retry it")
		return
	}
	// Settle even if the handler's ctx was cancelled.
	settleCtx, cancelSettle := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancelSettle()
	if err == nil {
		if cerr := r.Queue.Complete(settleCtx, job, r.WorkerID); cerr != nil {
			log.Error("complete failed", "err", cerr)
			return
		}
		log.InfoContext(ctx, "job succeeded")
		return
	}
	dead, ferr := r.Queue.Fail(settleCtx, job, r.WorkerID, err)
	if ferr == nil {
		observability.M().JobFailed(ctx, job.Kind, dead) // OBS-AC-005
	}
	switch {
	case ferr != nil:
		log.ErrorContext(ctx, "fail failed", "err", ferr, "cause", err)
	case dead:
		log.ErrorContext(ctx, "job dead-lettered", "err", err)
	default:
		log.WarnContext(ctx, "job failed; retry scheduled", "err", err)
	}
}

// run calls the handler, converting a panic into a job failure.
func (r *Runner) run(ctx context.Context, job Job) (err error) {
	h, ok := r.Handlers[job.Kind]
	if !ok {
		return Permanent(fmt.Errorf("no handler for kind %q", job.Kind))
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panic: %v", p)
		}
	}()
	return h(ctx, job, r.Tenant(job.TenantID))
}

func jittered(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d))) //nolint:gosec // jitter, not security
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
