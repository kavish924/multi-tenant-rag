package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/kavish924/multi-tenant-rag"

// Span attribute keys (design §5). Ids are fine on spans and logs, never on
// metrics.
const (
	AttrTenantID   = attribute.Key("tenant.id")
	AttrDocumentID = attribute.Key("document.id")
	AttrJobID      = attribute.Key("job.id")
	AttrJobKind    = attribute.Key("job.kind")
	AttrProvider   = attribute.Key("provider.name")
)

// Tracer is the platform tracer on the global provider.
func Tracer() trace.Tracer { return otel.Tracer(tracerName) }

// Start starts a span. End it with End, which records err.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// End records err (if any) on the span and ends it.
func End(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// SpanAt records a span that already happened (for example the claim that
// ran before the job's trace context was known).
func SpanAt(ctx context.Context, name string, start, end time.Time, attrs ...attribute.KeyValue) {
	_, span := Tracer().Start(ctx, name, trace.WithTimestamp(start), trace.WithAttributes(attrs...))
	span.End(trace.WithTimestamp(end))
}

// TraceParent returns the W3C traceparent for ctx's span, or "" (job
// payloads carry it, OBS-REQ-003).
func TraceParent(ctx context.Context) string {
	c := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, c)
	return c["traceparent"]
}

// WithTraceParent returns ctx with the remote span context from a
// traceparent value; an empty or invalid value leaves ctx unchanged.
func WithTraceParent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}

type requestIDKey struct{}

// WithRequestID stores the request id for logs.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the request id in ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// HeaderRequestID is accepted from callers and echoed on every response.
const HeaderRequestID = "X-Request-Id"

// Middleware starts the server span (continuing an inbound W3C
// traceparent), assigns the request id, echoes it in X-Request-Id, and
// records http_request_duration_seconds{method,route,status} once the mux
// has matched the route (OBS-REQ-001/004, design §5).
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		id := r.Header.Get(HeaderRequestID)
		if !validRequestID.MatchString(id) {
			id = newRequestID()
		}
		ctx = WithRequestID(ctx, id)
		ctx, span := Tracer().Start(ctx, r.Method, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", r.Method), attribute.String("request.id", id)))
		w.Header().Set(HeaderRequestID, id)

		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		r = r.WithContext(ctx)
		defer func() {
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			span.SetName(route)
			span.SetAttributes(attribute.String("http.route", route), attribute.Int("http.response.status_code", rec.status))
			if rec.status >= 500 {
				span.SetStatus(codes.Error, http.StatusText(rec.status))
			}
			span.End()
			M().HTTPRequest(ctx, r.Method, route, rec.status, time.Since(start))
		}()
		next.ServeHTTP(rec, r)
	})
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

// Flush keeps SSE streaming working through the middleware (spec 008).
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
