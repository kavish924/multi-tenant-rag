package quotas

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/observability"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
)

// Defaults for the limiter.
const (
	DefaultCacheTTL = 5 * time.Second
	// DefaultSlotTTL outlives the longest ask stream (Cloud Run timeout
	// 900 s), so a crashed instance's slot frees itself.
	DefaultSlotTTL = 16 * time.Minute
	// concurrencyRetryAfter is the hint when all ask slots are busy.
	concurrencyRetryAfter = 5 * time.Second
)

// Limiter checks and reserves quota. Every query runs inside the tenant's
// own WithTenant transaction (RLS applies).
type Limiter struct {
	store    *storage.Store
	defaults Limits
	// CacheTTL bounds how stale effective limits may be (design §7); an
	// override applies within this time (QTA-AC-007). Zero = default; a
	// negative value disables caching.
	CacheTTL time.Duration
	SlotTTL  time.Duration
	Now      func() time.Time

	mu    sync.Mutex
	cache map[uuid.UUID]cachedLimits
}

type cachedLimits struct {
	limits  Limits
	expires time.Time
}

// New builds a limiter with the given platform defaults.
func New(store *storage.Store, defaults Limits) *Limiter {
	return &Limiter{store: store, defaults: defaults, cache: map[uuid.UUID]cachedLimits{}}
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Limiter) cacheTTL() time.Duration {
	if l.CacheTTL == 0 {
		return DefaultCacheTTL
	}
	return l.CacheTTL
}

// Effective returns defaults overlaid with the tenant's overrides.
func (l *Limiter) Effective(ctx context.Context, tenantID uuid.UUID) (Limits, error) {
	now := l.now()
	if ttl := l.cacheTTL(); ttl > 0 {
		l.mu.Lock()
		c, ok := l.cache[tenantID]
		l.mu.Unlock()
		if ok && now.Before(c.expires) {
			return c.limits, nil
		}
	}
	var o Overrides
	err := l.store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT uploads_day, questions_day, embed_tokens_month, generate_tokens_month, concurrent_asks, concurrent_jobs
			FROM tenant_quotas WHERE tenant_id = $1`, tenantID).
			Scan(&o.UploadsDay, &o.QuestionsDay, &o.EmbedTokensMonth, &o.GenerateTokensMonth, &o.ConcurrentAsks, &o.ConcurrentJobs)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return Limits{}, fmt.Errorf("quotas: load: %w", err)
	}
	lim := o.Apply(l.defaults)
	if ttl := l.cacheTTL(); ttl > 0 {
		l.mu.Lock()
		l.cache[tenantID] = cachedLimits{limits: lim, expires: now.Add(ttl)}
		l.mu.Unlock()
	}
	return lim, nil
}

// Reservation is quota taken by Reserve; Release gives it back (an upload
// that failed validation, a duplicate, an ask that never started).
type Reservation struct {
	l      *Limiter
	tenant uuid.UUID
	dim    string
	n      int64
	window time.Time
	once   sync.Once
}

// Release refunds the reservation once; it ignores cancellation of ctx.
func (r *Reservation) Release(ctx context.Context) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = r.l.store.WithTenant(ctx, r.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE tenant_usage_counters SET used = greatest(used - $4, 0)
				WHERE tenant_id = $1 AND dimension = $2 AND window_start = $3`, r.tenant, r.dim, r.window, r.n)
			return err
		})
	})
}

// Reserve atomically takes n units of a counted dimension, or returns
// *Exceeded. The conditional UPSERT serializes on the counter row, so
// concurrent requests can never overdraw it (QTA-NFR-002: zero tolerance
// for counts, QTA-AC-005).
func (l *Limiter) Reserve(ctx context.Context, tenantID uuid.UUID, dim string, n int64) (*Reservation, error) {
	lim, err := l.Effective(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	limit := lim.LimitFor(dim)
	w := WindowFor(dim, l.now())
	var used int64
	granted := false
	err = l.store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO tenant_usage_counters AS c (tenant_id, dimension, window_start, used)
			SELECT $1, $2, $3, $4 WHERE $4 <= $5
			ON CONFLICT (tenant_id, dimension, window_start)
			DO UPDATE SET used = c.used + EXCLUDED.used WHERE c.used + EXCLUDED.used <= $5
			RETURNING used`, tenantID, dim, w.Start, n, limit).Scan(&used)
		if err == nil {
			granted = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT coalesce((SELECT used FROM tenant_usage_counters
			                 WHERE tenant_id = $1 AND dimension = $2 AND window_start = $3), 0)`,
			tenantID, dim, w.Start).Scan(&used)
	})
	if err != nil {
		return nil, fmt.Errorf("quotas: reserve %s: %w", dim, err)
	}
	if !granted {
		return nil, &Exceeded{Code: codeFor(dim), Limit: limit, Used: used, RetryAfter: w.End.Sub(l.now())}
	}
	return &Reservation{l: l, tenant: tenantID, dim: dim, n: n, window: w.Start}, nil
}

// CheckTokens rejects a call whose projected tokens would exceed the
// remaining monthly budget (QTA-AC-003). It reserves nothing: actual usage
// is added by AddTokens when the usage row is written, so concurrent calls
// can overshoot by at most (concurrent calls × projection) — bounded by the
// concurrency cap and documented as the token tolerance.
func (l *Limiter) CheckTokens(ctx context.Context, tenantID uuid.UUID, dim string, projected int64) error {
	lim, err := l.Effective(ctx, tenantID)
	if err != nil {
		return err
	}
	limit := lim.LimitFor(dim)
	w := WindowFor(dim, l.now())
	used, err := l.used(ctx, tenantID, dim, w.Start)
	if err != nil {
		return err
	}
	if used+projected > limit {
		return &Exceeded{Code: CodeTokens, Limit: limit, Used: used, RetryAfter: w.End.Sub(l.now())}
	}
	return nil
}

func (l *Limiter) used(ctx context.Context, tenantID uuid.UUID, dim string, window time.Time) (int64, error) {
	var used int64
	err := l.store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT coalesce((SELECT used FROM tenant_usage_counters
			                 WHERE tenant_id = $1 AND dimension = $2 AND window_start = $3), 0)`,
			tenantID, dim, window).Scan(&used)
	})
	if err != nil {
		return 0, fmt.Errorf("quotas: read %s: %w", dim, err)
	}
	return used, nil
}

// AddTokens adds actual usage to a token budget inside tx, the caller's
// WithTenant(tenantID) transaction. usage.Recorder calls it in the same
// transaction that inserts the usage_events row, so counters always equal
// the usage_events aggregates (QTA-AC-008).
func AddTokens(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, kind string, tokens int64, at time.Time) error {
	dim, ok := TokenDimension(kind)
	if !ok || tokens <= 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO tenant_usage_counters AS c (tenant_id, dimension, window_start, used)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, dimension, window_start) DO UPDATE SET used = c.used + EXCLUDED.used`,
		tenantID, dim, WindowFor(dim, at).Start, tokens)
	return err
}

// Slot is one in-flight ask stream.
type Slot struct {
	l      *Limiter
	tenant uuid.UUID
	id     uuid.UUID
	once   sync.Once
}

// Release frees the slot; it ignores cancellation of ctx.
func (s *Slot) Release(ctx context.Context) {
	if s == nil {
		return
	}
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.l.store.WithTenant(ctx, s.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM ask_slots WHERE id = $1`, s.id)
			return err
		})
	})
}

// AcquireAskSlot takes one of the tenant's concurrent_asks slots
// (QTA-AC-004). A transaction-scoped advisory lock per tenant serializes
// acquirers across API instances, so the cap is exact.
func (l *Limiter) AcquireAskSlot(ctx context.Context, tenantID uuid.UUID) (*Slot, error) {
	lim, err := l.Effective(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	ttl := l.SlotTTL
	if ttl <= 0 {
		ttl = DefaultSlotTTL
	}
	slot := &Slot{l: l, tenant: tenantID, id: uuid.New()}
	var live int64
	granted := false
	err = l.store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('ask_slots:' || $1::text, 0))`, tenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM ask_slots WHERE tenant_id = $1 AND expires_at < now()`, tenantID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ask_slots WHERE tenant_id = $1`, tenantID).Scan(&live); err != nil {
			return err
		}
		if live >= lim.ConcurrentAsks {
			return nil
		}
		granted = true
		_, err := tx.Exec(ctx, `INSERT INTO ask_slots (id, tenant_id, expires_at) VALUES ($1, $2, now() + $3::interval)`,
			slot.id, tenantID, ttl)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("quotas: ask slot: %w", err)
	}
	if !granted {
		return nil, &Exceeded{Code: CodeConcurrency, Limit: lim.ConcurrentAsks, Used: live, RetryAfter: concurrencyRetryAfter}
	}
	return slot, nil
}

// Admission is what an admitted ask holds until it ends.
type Admission struct {
	slot     *Slot
	question *Reservation
}

// Done releases the concurrency slot; the question stays counted.
func (a *Admission) Done(ctx context.Context) {
	if a != nil {
		a.slot.Release(ctx)
	}
}

// Abort releases everything (the ask never reached a provider).
func (a *Admission) Abort(ctx context.Context) {
	if a != nil {
		a.slot.Release(ctx)
		a.question.Release(ctx)
	}
}

// AdmitAsk runs the ask gate before any provider call (QTA-REQ-002):
// a concurrency slot, one question from questions/day, and the projected
// embedding and generation tokens against the monthly budgets.
func (l *Limiter) AdmitAsk(ctx context.Context, tenantID uuid.UUID, embedTokens, generateTokens int64) (*Admission, error) {
	slot, err := l.AcquireAskSlot(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if err := l.CheckTokens(ctx, tenantID, DimEmbedTokens, embedTokens); err != nil {
		slot.Release(ctx)
		return nil, err
	}
	if err := l.CheckTokens(ctx, tenantID, DimGenerateTokens, generateTokens); err != nil {
		slot.Release(ctx)
		return nil, err
	}
	q, err := l.Reserve(ctx, tenantID, DimQuestions, 1)
	if err != nil {
		slot.Release(ctx)
		return nil, err
	}
	return &Admission{slot: slot, question: q}, nil
}

// DimensionUsage is one row of the usage report.
type DimensionUsage struct {
	Limit       int64     `json:"limit"`
	Used        int64     `json:"used"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
}

// Report is GET /tenants/{tenant}/usage (QTA-REQ-006).
type Report struct {
	Dimensions       map[string]DimensionUsage `json:"dimensions"`
	ConcurrentAsks   DimensionUsage            `json:"concurrent_asks"`
	ConcurrentJobs   int64                     `json:"concurrent_jobs_limit"`
	EstimatedCostUSD float64                   `json:"estimated_cost_usd"`
	CostPeriodStart  time.Time                 `json:"cost_period_start"`
}

// Report returns current-window counters and limits, plus this month's
// estimated cost from usage_events (QTA-REQ-007).
func (l *Limiter) Report(ctx context.Context, tenantID uuid.UUID) (Report, error) {
	lim, err := l.Effective(ctx, tenantID)
	if err != nil {
		return Report{}, err
	}
	now := l.now()
	rep := Report{Dimensions: map[string]DimensionUsage{}, ConcurrentJobs: lim.ConcurrentJobs}
	month := WindowFor(DimGenerateTokens, now)
	rep.CostPeriodStart = month.Start
	err = l.store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		for _, dim := range []string{DimUploads, DimQuestions, DimEmbedTokens, DimGenerateTokens} {
			w := WindowFor(dim, now)
			var used int64
			if err := tx.QueryRow(ctx, `
				SELECT coalesce((SELECT used FROM tenant_usage_counters
				                 WHERE tenant_id = $1 AND dimension = $2 AND window_start = $3), 0)`,
				tenantID, dim, w.Start).Scan(&used); err != nil {
				return err
			}
			rep.Dimensions[dim] = DimensionUsage{Limit: lim.LimitFor(dim), Used: used, WindowStart: w.Start, WindowEnd: w.End}
		}
		var live int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ask_slots WHERE tenant_id = $1 AND expires_at >= now()`, tenantID).Scan(&live); err != nil {
			return err
		}
		rep.ConcurrentAsks = DimensionUsage{Limit: lim.ConcurrentAsks, Used: live}
		rows, err := tx.Query(ctx, `
			SELECT model, sum(input_tokens), sum(output_tokens) FROM usage_events
			WHERE tenant_id = $1 AND created_at >= $2 AND created_at < $3
			GROUP BY model`, tenantID, month.Start, month.End)
		if err != nil {
			return err
		}
		var model string
		var in, out int64
		_, err = pgx.ForEachRow(rows, []any{&model, &in, &out}, func() error {
			rep.EstimatedCostUSD += observability.EstimateCost(model, int(in), int(out))
			return nil
		})
		return err
	})
	if err != nil {
		return Report{}, fmt.Errorf("quotas: report: %w", err)
	}
	return rep, nil
}

// SetOverrides upserts a tenant's overrides (the operator path,
// QTA-REQ-008). It needs a connection allowed to write tenant_quotas — the
// migrator/owner role used by cmd/quotactl; the app role is read-only.
func SetOverrides(ctx context.Context, store *storage.Store, tenantID uuid.UUID, o Overrides) error {
	return store.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO tenant_quotas (tenant_id, uploads_day, questions_day, embed_tokens_month,
			                           generate_tokens_month, concurrent_asks, concurrent_jobs, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now())
			ON CONFLICT (tenant_id) DO UPDATE SET
			  uploads_day = EXCLUDED.uploads_day, questions_day = EXCLUDED.questions_day,
			  embed_tokens_month = EXCLUDED.embed_tokens_month, generate_tokens_month = EXCLUDED.generate_tokens_month,
			  concurrent_asks = EXCLUDED.concurrent_asks, concurrent_jobs = EXCLUDED.concurrent_jobs,
			  updated_at = now()`,
			tenantID, o.UploadsDay, o.QuestionsDay, o.EmbedTokensMonth, o.GenerateTokensMonth, o.ConcurrentAsks, o.ConcurrentJobs)
		return err
	})
}
