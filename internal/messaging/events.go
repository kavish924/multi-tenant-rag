// Package messaging is the transactional outbox (spec 006): producers Emit
// an event row inside the same transaction as the state change, the relay
// (cmd/outbox-relay) publishes committed rows through a Publisher, and
// consumers dedupe redeliveries with Consume.
//
// Delivery is at-least-once. Payloads carry ids only, never document text.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Topics (OBX-REQ-002).
const (
	TopicDocumentCreated  = "document.created"
	TopicDocumentIndexed  = "document.indexed"
	TopicDocumentFailed   = "document.failed"
	TopicDocumentDeleting = "document.deleting"
	TopicDocumentDeleted  = "document.deleted" // cleanup finished (013)
	TopicTenantPurged     = "tenant.purged"    // purge finished (013)
)

// SchemaVersion is stamped on every published envelope. Bump it when a
// topic's data changes incompatibly; add fields freely otherwise.
const SchemaVersion = 1

// Payload is a typed event body. Topic ties each struct to one topic, so a
// producer cannot emit a body under the wrong topic.
type Payload interface{ Topic() string }

// DocumentCreated is emitted in the upload transaction.
type DocumentCreated struct {
	DocumentID  uuid.UUID `json:"document_id"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
}

// DocumentIndexed is emitted in the Index transaction.
type DocumentIndexed struct {
	DocumentID uuid.UUID `json:"document_id"`
	Chunks     int       `json:"chunks"`
	EmbedModel string    `json:"embed_model"`
}

// DocumentFailed is emitted when ingestion gives up on a document: a
// permanent failure, or the last attempt of a retryable one.
type DocumentFailed struct {
	DocumentID uuid.UUID `json:"document_id"`
	Reason     string    `json:"reason"` // ingestion reason code, never the raw error
}

// DocumentDeleting is emitted in the soft-delete transaction.
type DocumentDeleting struct {
	DocumentID uuid.UUID `json:"document_id"`
}

func (DocumentCreated) Topic() string  { return TopicDocumentCreated }
func (DocumentIndexed) Topic() string  { return TopicDocumentIndexed }
func (DocumentFailed) Topic() string   { return TopicDocumentFailed }
func (DocumentDeleting) Topic() string { return TopicDocumentDeleting }
func (DocumentDeleted) Topic() string  { return TopicDocumentDeleted }
func (TenantPurged) Topic() string     { return TopicTenantPurged }

// DocumentDeleted is emitted when cleanup has removed a document's chunks
// and object (spec 013).
type DocumentDeleted struct {
	DocumentID uuid.UUID `json:"document_id"`
}

// TenantPurged is emitted when a tenant purge finishes (spec 013).
type TenantPurged struct {
	Documents int `json:"documents"`
}

// Emit writes an outbox row inside tx, which must be a WithTenant
// transaction for tenantID (forced RLS rejects any other tenant). The event
// exists only if tx commits (OBX-AC-001). It returns the event id.
func Emit(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, p Payload) (int64, error) {
	if tenantID == uuid.Nil || p == nil {
		return 0, errors.New("messaging: tenant id and payload are required")
	}
	data, err := json.Marshal(p)
	if err != nil {
		return 0, fmt.Errorf("messaging: payload: %w", err)
	}
	var id int64
	err = tx.QueryRow(ctx,
		`INSERT INTO outbox (tenant_id, topic, payload) VALUES ($1, $2, $3) RETURNING id`,
		tenantID, p.Topic(), data).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("messaging: emit %s: %w", p.Topic(), err)
	}
	return id, nil
}

// Event is one relayed outbox row (OBX-REQ-005).
type Event struct {
	ID         int64
	Topic      string
	TenantID   uuid.UUID
	OccurredAt time.Time
	Data       json.RawMessage
}

type envelope struct {
	EventID    int64           `json:"event_id"`
	Version    int             `json:"version"`
	Topic      string          `json:"topic"`
	TenantID   uuid.UUID       `json:"tenant_id"`
	OccurredAt time.Time       `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

// MarshalJSON is the wire format every publisher sends.
func (e Event) MarshalJSON() ([]byte, error) {
	data := e.Data
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	return json.Marshal(envelope{
		EventID: e.ID, Version: SchemaVersion, Topic: e.Topic,
		TenantID: e.TenantID, OccurredAt: e.OccurredAt.UTC(), Data: data,
	})
}

// UnmarshalJSON reads the wire format back (consumers, tests).
func (e *Event) UnmarshalJSON(b []byte) error {
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return err
	}
	*e = Event{ID: env.EventID, Topic: env.Topic, TenantID: env.TenantID, OccurredAt: env.OccurredAt, Data: env.Data}
	return nil
}

// Consume records that consumer handled ev, inside the consumer's own
// WithTenant(ev.TenantID) transaction. first is false for a redelivery: the
// caller then skips its side effect, and because the record and the side
// effect commit together, a duplicate never repeats it (OBX-AC-003).
func Consume(ctx context.Context, tx pgx.Tx, consumer string, ev Event) (first bool, err error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO consumed_events (consumer, event_id, tenant_id) VALUES ($1, $2, $3)
		ON CONFLICT (consumer, event_id) DO NOTHING`, consumer, ev.ID, ev.TenantID)
	if err != nil {
		return false, fmt.Errorf("messaging: consume: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
