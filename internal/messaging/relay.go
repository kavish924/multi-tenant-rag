package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Relay defaults (OBX-NFR-001: lag p95 < 5 s).
const (
	DefaultBatchSize    = 100
	DefaultPollInterval = time.Second
	DefaultMaxAttempts  = 5
	DefaultMaxBackoff   = time.Minute
	// LagWarning is the relay lag above which a batch is logged as a warning.
	LagWarning = 5 * time.Second
)

// relayLockKey is the session advisory lock that keeps one relay active
// (design §3). Any other instance waits on standby.
const relayLockKey int64 = 0x6f7574626f78 // "outbox"

// ErrNotLeader is returned by Tick when another relay holds the lock.
var ErrNotLeader = errors.New("messaging: another relay is active")

// Relay publishes committed outbox rows in id order (OBX-REQ-003). It reads
// and acks rows only through the outbox_next / outbox_mark_published /
// outbox_mark_failed system functions, so it runs as the ordinary app role.
type Relay struct {
	Pool         *pgxpool.Pool
	Publisher    Publisher
	BatchSize    int           // zero = DefaultBatchSize
	PollInterval time.Duration // zero = DefaultPollInterval
	MaxAttempts  int           // permanent rejections before quarantine; zero = DefaultMaxAttempts
	MaxBackoff   time.Duration // cap on the wait after publish failures; zero = DefaultMaxBackoff
	Log          *slog.Logger

	// Quarantined, when set, is called for each row taken out of the relay
	// after MaxAttempts permanent rejections (the alert hook for spec 010).
	Quarantined func(ev Event, cause error)

	// BeforeMark, when set, runs after a successful publish and before the
	// row is marked. An error stops the batch without marking, which is how
	// tests simulate a crash between publish and mark (OBX-AC-002).
	BeforeMark func(ev Event) error
}

// Stats summarizes one batch.
type Stats struct {
	Fetched     int
	Published   int
	Failed      int // transient publish errors (row retried later)
	Rejected    int // permanent publish errors below the quarantine limit
	Quarantined int
	Skipped     int // rows held back behind a failed row of the same tenant
	MaxLag      time.Duration
}

func (s Stats) failures() int { return s.Failed + s.Rejected }

// pending is one row from outbox_next.
type pending struct {
	Event
	attempts int
}

// source is the relay's view of the database; pgSource in production, a
// fake in unit tests.
type source interface {
	next(ctx context.Context, limit int) ([]pending, error)
	markPublished(ctx context.Context, id int64) (bool, error)
	markFailed(ctx context.Context, id int64, msg string, maxAttempts int) (bool, error)
}

type pgSource struct{ conn *pgx.Conn }

func (s pgSource) next(ctx context.Context, limit int) ([]pending, error) {
	rows, err := s.conn.Query(ctx,
		`SELECT id, tenant_id, topic, payload, created_at, attempts FROM outbox_next($1)`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (pending, error) {
		var p pending
		err := r.Scan(&p.ID, &p.TenantID, &p.Topic, &p.Data, &p.OccurredAt, &p.attempts)
		return p, err
	})
}

func (s pgSource) markPublished(ctx context.Context, id int64) (bool, error) {
	var ok bool
	err := s.conn.QueryRow(ctx, `SELECT outbox_mark_published($1)`, id).Scan(&ok)
	return ok, err
}

func (s pgSource) markFailed(ctx context.Context, id int64, msg string, maxAttempts int) (bool, error) {
	var q bool
	err := s.conn.QueryRow(ctx, `SELECT outbox_mark_failed($1, $2, $3)`, id, msg, maxAttempts).Scan(&q)
	return q, err
}

// Run relays until ctx is cancelled. It holds the relay lock on one
// dedicated connection and does all its work on that connection, so losing
// the connection also releases the lock; it then reconnects and competes for
// the lock again.
func (r *Relay) Run(ctx context.Context) error {
	if r.Pool == nil || r.Publisher == nil {
		return errors.New("messaging: relay needs a pool and a publisher")
	}
	for {
		err := r.lead(ctx)
		if ctx.Err() != nil {
			return nil
		}
		r.log().Error("outbox relay connection lost; reconnecting", "err", err)
		if !sleep(ctx, r.interval()) {
			return nil
		}
	}
}

func (r *Relay) lead(ctx context.Context) error {
	conn, err := r.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer destroy(conn)

	standby := false
	for {
		got, err := tryLock(ctx, conn.Conn())
		if err != nil {
			return err
		}
		if got {
			break
		}
		if !standby {
			r.log().Info("outbox relay on standby: another instance is active")
			standby = true
		}
		if !sleep(ctx, r.interval()) {
			return nil
		}
	}
	r.log().Info("outbox relay active")

	src := pgSource{conn.Conn()}
	failures := 0
	for {
		st, err := r.batch(ctx, src)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		r.report(st)
		wait := r.interval()
		switch {
		case st.failures() > 0:
			failures++
			wait = r.backoff(failures)
		case st.Fetched >= r.batchSize():
			failures = 0
			continue // more rows are waiting
		default:
			failures = 0
		}
		if !sleep(ctx, wait) {
			return nil
		}
	}
}

// Tick takes the relay lock, relays one batch and releases the lock. It
// returns ErrNotLeader when another relay is active. Run is the production
// loop; Tick serves tests and one-shot drains.
func (r *Relay) Tick(ctx context.Context) (Stats, error) {
	conn, err := r.Pool.Acquire(ctx)
	if err != nil {
		return Stats{}, err
	}
	got, err := tryLock(ctx, conn.Conn())
	if err != nil {
		destroy(conn)
		return Stats{}, err
	}
	if !got {
		conn.Release()
		return Stats{}, ErrNotLeader
	}
	st, err := r.batch(ctx, pgSource{conn.Conn()})
	if err != nil {
		destroy(conn)
		return st, err
	}
	if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, relayLockKey); err != nil {
		destroy(conn)
		return st, err
	}
	conn.Release()
	return st, nil
}

// batch publishes one batch in id order. Per-tenant order holds
// (OBX-AC-005): once a row of a tenant fails, the tenant's later rows in the
// batch are skipped, so they cannot overtake it. Only database errors are
// returned; publish failures are counted and retried on a later batch.
func (r *Relay) batch(ctx context.Context, src source) (Stats, error) {
	var st Stats
	rows, err := src.next(ctx, r.batchSize())
	if err != nil {
		return st, fmt.Errorf("messaging: outbox_next: %w", err)
	}
	st.Fetched = len(rows)
	blocked := map[uuid.UUID]bool{}
	for _, p := range rows {
		if ctx.Err() != nil {
			return st, nil
		}
		if blocked[p.TenantID] {
			st.Skipped++
			continue
		}
		perr := r.Publisher.Publish(ctx, p.Event)
		// A publish that went through is acked even during shutdown.
		ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if perr != nil {
			if ctx.Err() != nil {
				cancel()
				return st, nil
			}
			err := r.publishFailed(ackCtx, src, p, perr, &st, blocked)
			cancel()
			if err != nil {
				return st, err
			}
			continue
		}
		if r.BeforeMark != nil {
			if err := r.BeforeMark(p.Event); err != nil {
				cancel()
				return st, err
			}
		}
		ok, err := src.markPublished(ackCtx, p.ID)
		cancel()
		if err != nil {
			return st, fmt.Errorf("messaging: mark published %d: %w", p.ID, err)
		}
		if !ok {
			r.log().Warn("outbox row was already marked", "event_id", p.ID)
		}
		st.Published++
		if lag := time.Since(p.OccurredAt); lag > st.MaxLag {
			st.MaxLag = lag
		}
	}
	return st, nil
}

func (r *Relay) publishFailed(ctx context.Context, src source, p pending, perr error, st *Stats, blocked map[uuid.UUID]bool) error {
	log := r.log().With("event_id", p.ID, "topic", p.Topic, "tenant_id", p.TenantID)
	if !IsPermanent(perr) {
		st.Failed++
		blocked[p.TenantID] = true
		log.Warn("publish failed; will retry", "err", perr)
		return nil
	}
	quarantined, err := src.markFailed(ctx, p.ID, perr.Error(), r.maxAttempts())
	if err != nil {
		return fmt.Errorf("messaging: mark failed %d: %w", p.ID, err)
	}
	if !quarantined {
		st.Rejected++
		blocked[p.TenantID] = true
		log.Warn("publisher rejected event", "attempt", p.attempts+1, "err", perr)
		return nil
	}
	// The row is out of the relay; the tenant's later rows may proceed.
	st.Quarantined++
	log.Error("outbox event quarantined after repeated rejections", "attempts", p.attempts+1, "err", perr)
	if r.Quarantined != nil {
		r.Quarantined(p.Event, perr)
	}
	return nil
}

func (r *Relay) report(st Stats) {
	if st.Fetched == 0 {
		return
	}
	level := slog.LevelDebug
	if st.MaxLag > LagWarning || st.failures() > 0 || st.Quarantined > 0 {
		level = slog.LevelWarn
	}
	r.log().Log(context.Background(), level, "outbox batch",
		"fetched", st.Fetched, "published", st.Published, "failed", st.Failed,
		"rejected", st.Rejected, "quarantined", st.Quarantined, "skipped", st.Skipped,
		"max_lag_ms", st.MaxLag.Milliseconds())
}

// backoff doubles the poll interval per consecutive failing batch, capped.
func (r *Relay) backoff(failures int) time.Duration {
	d := r.interval()
	maxB := r.MaxBackoff
	if maxB <= 0 {
		maxB = DefaultMaxBackoff
	}
	for i := 1; i < failures && d < maxB; i++ {
		d *= 2
	}
	return min(d, maxB)
}

func (r *Relay) batchSize() int {
	if r.BatchSize <= 0 {
		return DefaultBatchSize
	}
	return r.BatchSize
}

func (r *Relay) interval() time.Duration {
	if r.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return r.PollInterval
}

func (r *Relay) maxAttempts() int {
	if r.MaxAttempts <= 0 {
		return DefaultMaxAttempts
	}
	return r.MaxAttempts
}

func (r *Relay) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func tryLock(ctx context.Context, c *pgx.Conn) (bool, error) {
	var got bool
	err := c.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, relayLockKey).Scan(&got)
	return got, err
}

// destroy closes the connection instead of returning it to the pool, so a
// session lock it may hold can never leak to another pool user.
func destroy(c *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Hijack().Close(ctx)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
