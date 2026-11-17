package cleanup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kavish924/multi-tenant-rag/internal/observability"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
	"github.com/kavish924/multi-tenant-rag/internal/storage/objectstore"
)

// Sweeper defaults (DEL-REQ-004/007).
const (
	DefaultRetention   = 30 * 24 * time.Hour
	DefaultInterval    = time.Hour
	DefaultOrphanGrace = time.Hour
)

// sweepLockKey keeps one sweeper running across worker instances.
const sweepLockKey int64 = 0x7377656570 // "sweep"

// ErrNotLeader means another worker is sweeping.
var ErrNotLeader = errors.New("cleanup: another sweeper is running")

// Sweeper removes deleted-document metadata past retention and orphan
// objects (objects whose document row no longer exists).
type Sweeper struct {
	Pool    *pgxpool.Pool
	Store   *storage.Store
	Objects objectstore.ObjectStore
	// Retention keeps 'deleted' metadata this long (RETENTION_DAYS).
	Retention time.Duration
	// OrphanGrace protects objects of uploads still in flight: an upload
	// stores the object before its row commits.
	OrphanGrace time.Duration
	Interval    time.Duration
	Log         *slog.Logger
	Now         func() time.Time
}

// Report counts one sweep.
type Report struct {
	Metadata int // document rows hard-deleted
	Orphans  int // orphan objects deleted
}

// Run sweeps every Interval until ctx ends.
func (s *Sweeper) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		rep, err := s.SweepOnce(ctx)
		switch {
		case errors.Is(err, ErrNotLeader), ctx.Err() != nil:
		case err != nil:
			s.log().ErrorContext(ctx, "sweep failed", "err", err)
		default:
			s.log().InfoContext(ctx, "sweep finished", "metadata_removed", rep.Metadata, "orphans_removed", rep.Orphans)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SweepOnce runs one sweep while holding the sweeper lock.
func (s *Sweeper) SweepOnce(ctx context.Context) (Report, error) {
	var rep Report
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return rep, err
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, sweepLockKey).Scan(&got); err != nil {
		conn.Release()
		return rep, err
	}
	if !got {
		conn.Release()
		return rep, ErrNotLeader
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, sweepLockKey)
		conn.Release()
	}()

	ctx, span := observability.Start(ctx, "sweep")
	defer func() { observability.End(span, err) }()
	if rep.Metadata, err = s.sweepMetadata(ctx); err != nil {
		return rep, err
	}
	if rep.Orphans, err = s.sweepOrphans(ctx); err != nil {
		return rep, err
	}
	observability.M().SweeperRemoved(ctx, "metadata", rep.Metadata)
	observability.M().SweeperRemoved(ctx, "orphan_object", rep.Orphans)
	return rep, nil
}

func (s *Sweeper) retention() time.Duration {
	if s.Retention <= 0 {
		return DefaultRetention
	}
	return s.Retention
}

// sweepMetadata hard-deletes 'deleted' rows past retention through the
// sweep_candidates / sweep_delete_document system functions.
func (s *Sweeper) sweepMetadata(ctx context.Context) (int, error) {
	removed := 0
	for {
		rows, err := s.Pool.Query(ctx, `SELECT tenant_id, document_id FROM sweep_candidates($1, 500)`, s.retention())
		if err != nil {
			return removed, fmt.Errorf("sweep: candidates: %w", err)
		}
		type cand struct{ tenant, doc uuid.UUID }
		cands, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cand, error) {
			var c cand
			err := r.Scan(&c.tenant, &c.doc)
			return c, err
		})
		if err != nil {
			return removed, err
		}
		for _, c := range cands {
			// The object should already be gone; delete again (idempotent).
			if err := s.Objects.Delete(ctx, objectstore.DocumentKey(c.tenant, c.doc)); err != nil {
				return removed, fmt.Errorf("sweep: object: %w", err)
			}
			var ok bool
			if err := s.Pool.QueryRow(ctx, `SELECT sweep_delete_document($1, $2)`, c.doc, s.retention()).Scan(&ok); err != nil {
				return removed, fmt.Errorf("sweep: delete: %w", err)
			}
			if ok {
				removed++
			}
		}
		if len(cands) < 500 {
			return removed, nil
		}
	}
}

// sweepOrphans deletes objects older than the grace period whose document
// row is gone. Existence is checked inside each object's own tenant.
func (s *Sweeper) sweepOrphans(ctx context.Context) (int, error) {
	lister, ok := s.Objects.(objectstore.Lister)
	if !ok {
		return 0, nil // stores without listing rely on lifecycle rules
	}
	grace := s.OrphanGrace
	if grace <= 0 {
		grace = DefaultOrphanGrace
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	byTenant := map[uuid.UUID][]uuid.UUID{}
	err := lister.List(ctx, func(o objectstore.ObjectInfo) error {
		if now.Sub(o.Modified) < grace {
			return nil
		}
		tenant, doc, err := objectstore.ParseDocumentKey(o.Key)
		if err == nil {
			byTenant[tenant] = append(byTenant[tenant], doc)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("sweep: list objects: %w", err)
	}
	removed := 0
	for tenant, docs := range byTenant {
		existing := map[uuid.UUID]bool{}
		err := s.Store.WithTenant(ctx, tenant, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id FROM documents WHERE id = ANY($1)`, docs)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			for _, id := range ids {
				existing[id] = true
			}
			return err
		})
		if err != nil {
			return removed, fmt.Errorf("sweep: check rows: %w", err)
		}
		for _, doc := range docs {
			if existing[doc] {
				continue
			}
			if err := s.Objects.Delete(ctx, objectstore.DocumentKey(tenant, doc)); err != nil {
				return removed, fmt.Errorf("sweep: orphan: %w", err)
			}
			removed++
		}
	}
	return removed, nil
}

func (s *Sweeper) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}
