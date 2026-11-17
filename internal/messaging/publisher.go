package messaging

import (
	"context"
	"errors"
	"log/slog"
)

// Publisher delivers one event to a broker (OBX-REQ-004). It returns nil
// only once the broker has accepted the event; the relay then marks the row
// published. Return Permanent(err) when the broker rejects this event itself
// (retrying it cannot help); any other error is treated as an outage and
// retried without counting against the row.
type Publisher interface {
	Publish(ctx context.Context, ev Event) error
}

// LogPublisher is the default publisher when no broker is configured
// (OBX-AC-004): it writes each event to the log and accepts it.
type LogPublisher struct{ Log *slog.Logger }

// Publish implements Publisher.
func (p LogPublisher) Publish(ctx context.Context, ev Event) error {
	log := p.Log
	if log == nil {
		log = slog.Default()
	}
	log.LogAttrs(ctx, slog.LevelInfo, "event published",
		slog.Int64("event_id", ev.ID),
		slog.String("topic", ev.Topic),
		slog.String("tenant_id", ev.TenantID.String()),
		slog.Time("occurred_at", ev.OccurredAt),
		slog.String("event_data", string(ev.Data)), // ids only (OBX-REQ-005)
	)
	return nil
}

type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

// Permanent marks a publish error as caused by the event itself (for
// example a payload the broker rejects). Repeated permanent failures
// quarantine the row (design §7).
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether err was wrapped with Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}
