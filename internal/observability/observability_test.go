package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var (
	spans  = tracetest.NewSpanRecorder()
	reader = sdkmetric.NewManualReader()
)

func TestMain(m *testing.M) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(append(MeterOptions(), sdkmetric.WithReader(reader))...))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	m.Run()
}

func collect(t *testing.T) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out []metricdata.Metrics
	for _, sm := range rm.ScopeMetrics {
		out = append(out, sm.Metrics...)
	}
	return out
}

// attrSets returns every attribute set recorded for every metric.
func attrSets(ms []metricdata.Metrics) map[string][]attribute.Set {
	out := map[string][]attribute.Set{}
	for _, m := range ms {
		switch d := m.Data.(type) {
		case metricdata.Sum[int64]:
			for _, p := range d.DataPoints {
				out[m.Name] = append(out[m.Name], p.Attributes)
			}
		case metricdata.Sum[float64]:
			for _, p := range d.DataPoints {
				out[m.Name] = append(out[m.Name], p.Attributes)
			}
		case metricdata.Histogram[float64]:
			for _, p := range d.DataPoints {
				out[m.Name] = append(out[m.Name], p.Attributes)
			}
		case metricdata.Gauge[int64]:
			for _, p := range d.DataPoints {
				out[m.Name] = append(out[m.Name], p.Attributes)
			}
		case metricdata.Gauge[float64]:
			for _, p := range d.DataPoints {
				out[m.Name] = append(out[m.Name], p.Attributes)
			}
		}
	}
	return out
}

var uuidLike = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-`)

// OBS-AC-003 (the CI cardinality lint): every instrument, exercised, uses
// only allowlisted labels; identifiers appear only as tenant_id; and a
// forbidden label added by mistake is scrubbed by the view.
func TestMetricCardinality(t *testing.T) {
	ctx := context.Background()
	m := M()
	tenant := uuid.New()
	m.HTTPRequest(ctx, "POST", "POST /ask", 200, 120*time.Millisecond)
	m.JobAttempt(ctx, "ingest")
	m.JobFailed(ctx, "ingest", true)
	m.JobFailed(ctx, "ingest", false)
	m.ProviderError(ctx, "fake-hash-v1", "embed")
	m.RerankFallback(ctx, "timeout")
	m.IngestFailure(ctx, "extract_failed")
	m.Usage(ctx, tenant, "generate", "gemini-2.5-flash", 1000, 200)
	if err := m.RegisterQueueGauges(func(context.Context) ([]QueueStat, error) {
		return []QueueStat{{Kind: "ingest", Depth: 3, OldestAgeS: 12}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	// A developer mistake: a document id as a label.
	bad, _ := otel.GetMeterProvider().Meter("test").Int64Counter("mistake")
	bad.Add(ctx, 1, metricWith(attribute.String("document_id", uuid.NewString()), attribute.String("kind", "x")))

	sets := attrSets(collect(t))
	for _, name := range []string{"http_request_duration", "jobs_attempts", "jobs_failed", "provider_errors", "rerank_fallbacks",
		"ingest_failures", "tokens", "estimated_cost_usd", "jobs_queue_depth", "jobs_queue_age", "mistake"} {
		if len(sets[name]) == 0 {
			t.Errorf("metric %s not collected", name)
		}
	}
	for name, ss := range sets {
		for _, s := range ss {
			for _, kv := range s.ToSlice() {
				if !slices.Contains(AllowedLabels, string(kv.Key)) {
					t.Errorf("%s: forbidden label %q", name, kv.Key)
				}
				if kv.Key != "tenant_id" && uuidLike.MatchString(kv.Value.Emit()) {
					t.Errorf("%s: identifier in label %s=%s", name, kv.Key, kv.Value.Emit())
				}
			}
		}
	}
	if got := EstimateCost("gemini-2.5-flash", 1000, 200); got < 0.0007 || got > 0.0009 {
		t.Errorf("cost estimate %v", got)
	}
	if EstimateCost("fake-generator-v1", 1e6, 1e6) != 0 {
		t.Error("fakes must cost nothing")
	}
}

// OBS-AC-004 / OBS-REQ-006: correlation fields present, content redacted.
func TestLoggerCorrelationAndRedaction(t *testing.T) {
	projectID = "demo-project"
	defer func() { projectID = "" }()
	var buf bytes.Buffer
	log := NewLogger(&buf, nil)
	ctx, span := Start(WithRequestID(context.Background(), "req-123"), "op")
	log.InfoContext(ctx, "hello", "tenant_id", "t1",
		"content", "CANARY document text", "question", "CANARY question",
		"Authorization", "Bearer CANARY", "nested.prompt", "CANARY prompt")
	log.WithGroup("g").Info("grouped", "answer", "CANARY answer")
	span.End()

	out := buf.String()
	if strings.Contains(out, "CANARY") {
		t.Fatalf("redacted value leaked:\n%s", out)
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &line); err != nil {
		t.Fatal(err)
	}
	sc := span.SpanContext()
	want := map[string]any{
		"severity": "INFO", "message": "hello", "request_id": "req-123", "tenant_id": "t1",
		"logging.googleapis.com/trace":  "projects/demo-project/traces/" + sc.TraceID().String(),
		"logging.googleapis.com/spanId": sc.SpanID().String(),
		"content":                       Redacted,
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
}

func spanByName(name string) sdktrace.ReadOnlySpan {
	for _, s := range spans.Ended() {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// OBS-REQ-001 / design §5: request ids, inbound traceparent, route-named
// server span, latency histogram by route.
func TestMiddleware(t *testing.T) {
	mux := http.NewServeMux()
	var seenID string
	var innerTrace string
	mux.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, r *http.Request) {
		seenID = RequestID(r.Context())
		_, s := Start(r.Context(), "inner")
		innerTrace = s.SpanContext().TraceID().String()
		s.End()
		w.WriteHeader(http.StatusTeapot)
	})
	srv := httptest.NewServer(Middleware(mux))
	defer srv.Close()

	// Generated id, echoed.
	res, err := http.Get(srv.URL + "/things/1")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if id := res.Header.Get(HeaderRequestID); id == "" || id != seenID {
		t.Errorf("request id %q, handler saw %q", id, seenID)
	}

	// Caller-supplied id and traceparent are honoured.
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	req, _ := http.NewRequest("GET", srv.URL+"/things/2", nil)
	req.Header.Set(HeaderRequestID, "client-abc")
	req.Header.Set("traceparent", tp)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get(HeaderRequestID) != "client-abc" || innerTrace != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("id %q trace %s", res.Header.Get(HeaderRequestID), innerTrace)
	}

	// An unsafe id is replaced.
	req, _ = http.NewRequest("GET", srv.URL+"/things/3", nil)
	req.Header.Set(HeaderRequestID, "bad id with spaces")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := res.Header.Get(HeaderRequestID); got == "bad id with spaces" || got == "" {
		t.Error("unsafe request id echoed")
	}

	s := spanByName("GET /things/{id}")
	if s == nil {
		t.Fatal("server span not named by route")
	}
	found := false
	for _, set := range attrSets(collect(t))["http_request_duration"] {
		r, _ := set.Value("route")
		st, _ := set.Value("status")
		if r.AsString() == "GET /things/{id}" && st.AsInt64() == http.StatusTeapot {
			found = true
		}
	}
	if !found {
		t.Error("latency histogram lacks route/status labels")
	}
}

func TestTraceParentRoundTrip(t *testing.T) {
	ctx, span := Start(context.Background(), "upload")
	tp := TraceParent(ctx)
	span.End()
	if tp == "" {
		t.Fatal("no traceparent for a sampled span")
	}
	_, child := Start(WithTraceParent(context.Background(), tp), "job")
	child.End()
	if child.SpanContext().TraceID() != span.SpanContext().TraceID() {
		t.Error("job span not in the upload trace")
	}
	if TraceParent(context.Background()) != "" || WithTraceParent(context.Background(), "garbage") == nil {
		t.Error("empty/invalid traceparent handling")
	}
}

// OBS-AC-006: with the collector unreachable, span creation stays fast and
// shutdown gives up within its deadline.
func TestCollectorDownDoesNotBlock(t *testing.T) {
	prevT, prevM := otel.GetTracerProvider(), otel.GetMeterProvider()
	defer func() { otel.SetTracerProvider(prevT); otel.SetMeterProvider(prevM) }()

	shutdown, err := Setup(context.Background(), Config{Service: "test", Endpoint: "127.0.0.1:1", ExportTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 5000; i++ {
		_, s := Start(context.Background(), "work")
		s.End()
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("5000 spans took %v with the collector down", d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start = time.Now()
	_ = shutdown(ctx) // export errors are expected; blocking is not
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("shutdown blocked %v", d)
	}
}

func metricWith(kv ...attribute.KeyValue) metric.AddOption { return metric.WithAttributes(kv...) }
