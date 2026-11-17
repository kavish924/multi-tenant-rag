package jobs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/kavish924/multi-tenant-rag/internal/observability"
)

func TestTraceParentPayload(t *testing.T) {
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	out := withTraceParent([]byte(`{"document_id":"d1"}`), tp)
	var m map[string]string
	if err := json.Unmarshal(out, &m); err != nil || m["document_id"] != "d1" || m["traceparent"] != tp {
		t.Errorf("payload %s", out)
	}
	if traceParentOf(out) != tp {
		t.Error("traceparent not read back")
	}
	for _, p := range []string{`[1,2]`, `"x"`, `null`} {
		if string(withTraceParent([]byte(p), tp)) != p {
			t.Errorf("non-object payload %s changed", p)
		}
	}
	if string(withTraceParent([]byte(`{}`), "")) != `{}` || traceParentOf([]byte(`{}`)) != "" {
		t.Error("absent trace context")
	}
}

// OBS-AC-002: the worker's job span continues the enqueuing trace, and the
// handler's spans nest under it after a recorded claim span.
func TestRunnerContinuesEnqueueTrace(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer otel.SetTracerProvider(prev)

	upCtx, upload := observability.Start(context.Background(), "POST /documents")
	payload := withTraceParent([]byte(`{"document_id":"d"}`), observability.TraceParent(upCtx))
	upload.End()

	q := newFakeQueue(0)
	q.pending = []Job{{ID: uuid.New(), TenantID: uuid.New(), Kind: "k", Attempts: 1, MaxAttempts: 5, Payload: payload}}
	r := newRunner(q, func(ctx context.Context, _ Job, _ TenantTx) error {
		_, s := observability.Start(ctx, "extract")
		s.End()
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	waitFor(t, func() bool { return q.settled() == 1 })
	cancel()
	<-done

	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range rec.Ended() {
		byName[s.Name()] = s
	}
	job, claim, extract := byName["job k"], byName["claim"], byName["extract"]
	if job == nil || claim == nil || extract == nil {
		t.Fatalf("spans %v", byName)
	}
	trace := upload.SpanContext().TraceID()
	if job.SpanContext().TraceID() != trace || job.Parent().SpanID() != upload.SpanContext().SpanID() {
		t.Error("job span is not a child of the upload span")
	}
	if claim.Parent().SpanID() != job.SpanContext().SpanID() || extract.Parent().SpanID() != job.SpanContext().SpanID() {
		t.Error("claim/extract not children of the job span")
	}
	if job.StartTime().After(claim.StartTime()) {
		t.Error("job span starts after its claim")
	}
}
