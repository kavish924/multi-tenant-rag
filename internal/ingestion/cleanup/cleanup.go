// Package cleanup completes deletions (spec 013): the cleanup job removes a
// soft-deleted document's chunks, cache entries and stored object and marks
// it deleted; the purge job does that for a whole tenant; the sweeper
// hard-deletes metadata past retention and removes orphan objects.
//
// Races with ingestion are settled by the document row lock, not the queue:
// Delete and the Index transaction both lock the row, so either Index's
// chunks exist before this job can run (and are removed here), or Index
// sees deleted_at and writes nothing (ING-AC-005/009). The final invariant
// below checks it.
package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/jobs"
	"github.com/kavish924/multi-tenant-rag/internal/messaging"
	"github.com/kavish924/multi-tenant-rag/internal/observability"
	"github.com/kavish924/multi-tenant-rag/internal/storage/objectstore"
)

// Job kinds handled here (enqueued by internal/documents).
const (
	JobCleanup = "cleanup"
	JobPurge   = "purge"
)

// DefaultBatchSize bounds each chunk-delete transaction.
const DefaultBatchSize = 1000

// ErrChunksRemain is the failed final invariant: it signals a bug, so the
// job retries and an alert is logged.
var ErrChunksRemain = errors.New("cleanup: chunks remain after cleanup")

// CachePurger removes cached answers or embeddings derived from a document.
// No cache exists yet, so the default is a no-op; when one lands, its keys
// must include the tenant (design §10).
type CachePurger interface {
	PurgeDocument(ctx context.Context, tenantID, documentID uuid.UUID) error
}

// NoCache is the no-op CachePurger.
type NoCache struct{}

// PurgeDocument implements CachePurger.
func (NoCache) PurgeDocument(context.Context, uuid.UUID, uuid.UUID) error { return nil }

// Cleaner runs cleanup jobs.
type Cleaner struct {
	Objects   objectstore.ObjectStore
	Cache     CachePurger // nil = NoCache
	BatchSize int
	Log       *slog.Logger
}

type docPayload struct {
	DocumentID uuid.UUID `json:"document_id"`
}

// Handle implements jobs.Handler for JobCleanup.
func (c *Cleaner) Handle(ctx context.Context, job jobs.Job, tx jobs.TenantTx) error {
	var p docPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.DocumentID == uuid.Nil {
		return jobs.Permanent(fmt.Errorf("cleanup: bad payload: %s", job.Payload))
	}
	return c.CleanDocument(ctx, tx, job.TenantID, p.DocumentID)
}

// CleanDocument is idempotent: running it again after success, or after a
// partial failure, converges to the same final state (DEL-AC-004/005).
func (c *Cleaner) CleanDocument(ctx context.Context, tx jobs.TenantTx, tenantID, docID uuid.UUID) error {
	log := c.log().With("tenant_id", tenantID, "document_id", docID)
	ctx, span := observability.Start(ctx, "cleanup", observability.AttrDocumentID.String(docID.String()))
	var err error
	defer func() { observability.End(span, err) }()

	// 1. The document must be soft-deleted; a missing row was already swept.
	var deleted bool
	err = tx(ctx, func(t pgx.Tx) error {
		return t.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM documents WHERE id = $1`, docID).Scan(&deleted)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
		log.InfoContext(ctx, "cleanup: document already removed")
		return nil
	}
	if err != nil {
		return fmt.Errorf("cleanup: load: %w", err)
	}
	if !deleted {
		err = jobs.Permanent(errors.New("cleanup: document is not deleted"))
		log.ErrorContext(ctx, "cleanup job for a live document; refusing")
		return err
	}

	// 2. Chunks, explicitly and in batches (vectors live in the same rows);
	// ON DELETE CASCADE never fires here because the row is only soft-deleted.
	batch := c.BatchSize
	if batch <= 0 {
		batch = DefaultBatchSize
	}
	removed := int64(0)
	for {
		var n int64
		err = tx(ctx, func(t pgx.Tx) error {
			tag, err := t.Exec(ctx, `
				DELETE FROM chunks WHERE id IN (
				  SELECT id FROM chunks WHERE document_id = $1 LIMIT $2)`, docID, batch)
			n = tag.RowsAffected()
			return err
		})
		if err != nil {
			return fmt.Errorf("cleanup: delete chunks: %w", err)
		}
		removed += n
		if n < int64(batch) {
			break
		}
	}

	// 3. Derived cache entries.
	cache := c.Cache
	if cache == nil {
		cache = NoCache{}
	}
	if err = cache.PurgeDocument(ctx, tenantID, docID); err != nil {
		return fmt.Errorf("cleanup: cache: %w", err)
	}

	// 4. The stored object, addressed only through DocumentKey (no key from
	// the row, so nothing can be forged). Delete is idempotent; a failure
	// retries through the job ladder (DEL-AC-004).
	if err = c.Objects.Delete(ctx, objectstore.DocumentKey(tenantID, docID)); err != nil {
		return fmt.Errorf("cleanup: delete object: %w", err)
	}

	// 5. Invariant, then the terminal status, under the row lock.
	err = tx(ctx, func(t pgx.Tx) error {
		if _, err := t.Exec(ctx, `SELECT 1 FROM documents WHERE id = $1 FOR UPDATE`, docID); err != nil {
			return err
		}
		var left int64
		if err := t.QueryRow(ctx, `SELECT count(*) FROM chunks WHERE document_id = $1`, docID).Scan(&left); err != nil {
			return err
		}
		if left > 0 {
			return fmt.Errorf("%w: %d", ErrChunksRemain, left)
		}
		tag, err := t.Exec(ctx, `
			UPDATE documents SET status = 'deleted'
			WHERE id = $1 AND deleted_at IS NOT NULL AND status <> 'deleted'`, docID)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = messaging.Emit(ctx, t, tenantID, messaging.DocumentDeleted{DocumentID: docID})
		return err
	})
	if errors.Is(err, ErrChunksRemain) {
		log.ErrorContext(ctx, "ALERT cleanup: chunks remain after cleanup", "err", err)
	}
	if err != nil {
		return fmt.Errorf("cleanup: finish: %w", err)
	}
	log.InfoContext(ctx, "document cleaned up", "chunks_removed", removed)
	return nil
}

func (c *Cleaner) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}
