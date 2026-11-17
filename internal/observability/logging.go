package observability

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// RedactedKeys are attribute keys whose values never reach a log line
// (OBS-REQ-006): document text, prompts, answers, questions and secrets.
// Matching is case-insensitive on the last path segment of the key.
var RedactedKeys = []string{
	"content", "text", "body", "prompt", "answer", "question", "query", "data",
	"password", "password_hash", "secret", "token", "access_token", "authorization", "cookie", "api_key", "key",
}

// Redacted replaces a redacted value.
const Redacted = "[redacted]"

// NewLogger returns a JSON logger for Cloud Logging: "severity" and
// "message" field names, the request id and the trace/span ids of ctx on
// every *Context call (OBS-AC-004), and redaction of RedactedKeys.
func NewLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	})
	return slog.New(&contextHandler{Handler: h})
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 {
		switch a.Key {
		case slog.LevelKey:
			lvl, _ := a.Value.Any().(slog.Level)
			return slog.String("severity", severity(lvl))
		case slog.MessageKey:
			a.Key = "message"
			return a
		case slog.TimeKey:
			a.Key = "time"
			return a
		}
	}
	if isRedacted(a.Key) && a.Value.Kind() != slog.KindGroup {
		return slog.String(a.Key, Redacted)
	}
	return a
}

func isRedacted(key string) bool {
	k := strings.ToLower(key)
	if i := strings.LastIndexAny(k, "./"); i >= 0 {
		k = k[i+1:]
	}
	for _, r := range RedactedKeys {
		if k == r {
			return true
		}
	}
	return false
}

func severity(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARNING"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// contextHandler adds correlation fields from the record's context.
type contextHandler struct{ slog.Handler }

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		tid := sc.TraceID().String()
		if projectID != "" {
			tid = "projects/" + projectID + "/traces/" + tid
		}
		r.AddAttrs(
			slog.String("logging.googleapis.com/trace", tid),
			slog.String("logging.googleapis.com/spanId", sc.SpanID().String()),
			slog.Bool("logging.googleapis.com/trace_sampled", sc.IsSampled()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(as)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}
