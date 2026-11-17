package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/jobs"
	"github.com/kavish924/multi-tenant-rag/internal/messaging"
	"github.com/kavish924/multi-tenant-rag/internal/storage"
)

// Purger runs tenant purge jobs (DEL-REQ-005). Everything happens inside
// the purged tenant's own transactions; objects are addressed only through
// DocumentKey. The tenant row, users and memberships are retained (policy:
// retain; anonymization is out of scope), so the owner can still sign in
// and see an empty tenant.
type Purger struct {
	Cleaner *Cleaner
	// SoftDeleteBatch bounds each soft-delete transaction (default 200).
	SoftDeleteBatch int
}

// Handle implements jobs.Handler for JobPurge. It is resumable: a retry
// after a partial failure skips what is already done (design §7).
func (p *Purger) Handle(ctx context.Context, job jobs.Job, tx jobs.TenantTx) error {
	tenantID := job.TenantID
	batch := p.SoftDeleteBatch
	if batch <= 0 {
		batch = 200
	}

	// 1. Soft-delete every live document exactly as a user delete would:
	// deleted_at + 'deleting', a document.deleting event and a cleanup job,
	// so the content is excluded from search at once.
	for {
		var n int
		err := tx(ctx, func(t pgx.Tx) error {
			rows, err := t.Query(ctx, `
				UPDATE documents SET deleted_at = now(), status = 'deleting'
				WHERE id IN (SELECT id FROM documents WHERE deleted_at IS NULL LIMIT $1 FOR UPDATE)
				RETURNING id`, batch)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			if err != nil {
				return err
			}
			n = len(ids)
			for _, id := range ids {
				if _, _, err := jobs.Enqueue(ctx, t, jobs.EnqueueParams{
					TenantID: tenantID, Kind: JobCleanup,
					Payload:        map[string]string{"document_id": id.String()},
					IdempotencyKey: JobCleanup + ":" + id.String(),
				}); err != nil {
					return err
				}
				if _, err := messaging.Emit(ctx, t, tenantID, messaging.DocumentDeleting{DocumentID: id}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("purge: soft delete: %w", err)
		}
		if n < batch {
			break
		}
	}

	// 2. Clean every document still in 'deleting' now, so a finished purge
	// means no chunks and no objects (DEL-AC-006). The cleanup jobs from
	// step 1 then find nothing left to do.
	var pending []uuid.UUID
	err := tx(ctx, func(t pgx.Tx) error {
		rows, err := t.Query(ctx, `SELECT id FROM documents WHERE status = 'deleting'`)
		if err != nil {
			return err
		}
		pending, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	if err != nil {
		return fmt.Errorf("purge: list: %w", err)
	}
	var failed []error
	for _, id := range pending {
		if err := p.Cleaner.CleanDocument(ctx, tx, tenantID, id); err != nil {
			failed = append(failed, fmt.Errorf("document %s: %w", id, err))
		}
	}
	if len(failed) > 0 {
		// Retry resumes from the failed set: cleaned documents are 'deleted'.
		return fmt.Errorf("purge: %d of %d documents failed: %w", len(failed), len(pending), errors.Join(failed...))
	}

	// 3. Usage rows and counters, then the completion event.
	err = tx(ctx, func(t pgx.Tx) error {
		if _, err := t.Exec(ctx, `DELETE FROM usage_events WHERE tenant_id = $1`, tenantID); err != nil {
			return err
		}
		if _, err := t.Exec(ctx, `DELETE FROM tenant_usage_counters WHERE tenant_id = $1`, tenantID); err != nil {
			return err
		}
		_, err := messaging.Emit(ctx, t, tenantID, messaging.TenantPurged{Documents: len(pending)})
		return err
	})
	if err != nil {
		return fmt.Errorf("purge: usage: %w", err)
	}
	p.Cleaner.log().InfoContext(ctx, "tenant purged", "tenant_id", tenantID, "documents", len(pending))
	return nil
}

// ConsumerName is the consumed_events key of the reference consumer.
const ConsumerName = "cleanup"

// DeletingConsumer is the idempotent-consumer reference implementation
// (DEL-REQ-006, OBX-REQ-006): on document.deleting it ensures the cleanup
// job exists. It records the event id in the same tenant transaction as its
// side effect, so a redelivered event does nothing (DEL-AC-005). It is a
// messaging.Publisher, so the relay can deliver to it in-process; production
// enqueues cleanup in the delete transaction itself, so this consumer is a
// safety net, not the primary path.
type DeletingConsumer struct {
	Store *storage.Store
}

// Publish implements messaging.Publisher.
func (c DeletingConsumer) Publish(ctx context.Context, ev messaging.Event) error {
	if ev.Topic != messaging.TopicDocumentDeleting {
		return nil
	}
	var p docPayload
	if err := json.Unmarshal(ev.Data, &p); err != nil || p.DocumentID == uuid.Nil {
		return messaging.Permanent(fmt.Errorf("cleanup consumer: bad event %d", ev.ID))
	}
	return c.Store.WithTenant(ctx, ev.TenantID, func(tx pgx.Tx) error {
		first, err := messaging.Consume(ctx, tx, ConsumerName, ev)
		if err != nil || !first {
			return err // a duplicate: no side effect
		}
		_, _, err = jobs.Enqueue(ctx, tx, jobs.EnqueueParams{
			TenantID: ev.TenantID, Kind: JobCleanup,
			Payload:        map[string]string{"document_id": p.DocumentID.String()},
			IdempotencyKey: JobCleanup + ":" + p.DocumentID.String(),
		})
		return err
	})
}
