// Package usage records provider token usage per tenant (usage_events,
// GEN-REQ-007). Quotas (011) and cost metrics (010) read these rows.
package usage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kavish924/multi-tenant-rag/internal/storage"
)

// Kinds (usage_events.kind CHECK).
const (
	KindEmbed    = "embed"
	KindGenerate = "generate"
	KindRerank   = "rerank"
)

// Outcomes (usage_events.outcome CHECK).
const (
	OutcomeOK        = "ok"
	OutcomeError     = "error"
	OutcomeCancelled = "cancelled"
)

// Event is one provider call's usage.
type Event struct {
	TenantID     uuid.UUID
	Kind         string
	Model        string
	InputTokens  int
	OutputTokens int
	Outcome      string // zero = OutcomeOK
	RequestID    string
}

// Recorder writes usage rows.
type Recorder struct{ store *storage.Store }

// NewRecorder builds a recorder.
func NewRecorder(store *storage.Store) *Recorder { return &Recorder{store: store} }

// Record inserts one row in the event's tenant. It ignores cancellation of
// ctx (usage must be written even after a client disconnect) but gives up
// after 5 seconds.
func (r *Recorder) Record(ctx context.Context, e Event) error {
	if e.TenantID == uuid.Nil || e.Kind == "" || e.Model == "" {
		return errors.New("usage: tenant, kind and model are required")
	}
	if e.Outcome == "" {
		e.Outcome = OutcomeOK
	}
	var reqID any
	if e.RequestID != "" {
		reqID = e.RequestID
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := r.store.WithTenant(ctx, e.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO usage_events (tenant_id, kind, model, input_tokens, output_tokens, outcome, request_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			e.TenantID, e.Kind, e.Model, max(e.InputTokens, 0), max(e.OutputTokens, 0), e.Outcome, reqID)
		return err
	})
	if err != nil {
		return fmt.Errorf("usage: record: %w", err)
	}
	return nil
}
