package observability

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// AllowedLabels is the closed set of metric attribute keys (OBS-REQ-005).
// Every value is bounded: routes, job kinds, statuses, provider/model
// names, tenant ids (hundreds), token direction, fallback reasons and call
// outcomes. Document, user and chunk ids and any text are forbidden; the
// views below drop any other key at runtime and the cardinality test fails
// CI if code tries to use one.
var AllowedLabels = []string{"route", "method", "status", "kind", "provider", "tenant_id", "direction", "reason", "outcome", "stage", "verdict"}

// MeterOptions are the views every meter provider uses: an attribute
// allowlist on all instruments, plus latency buckets for the HTTP histogram.
func MeterOptions() []sdkmetric.Option {
	allowed := attribute.NewAllowKeysFilter(keys(AllowedLabels)...)
	// One view for every instrument (two matching views would duplicate
	// streams): the allowlist everywhere, latency buckets on the histogram.
	view := func(i sdkmetric.Instrument) (sdkmetric.Stream, bool) {
		s := sdkmetric.Stream{Name: i.Name, Description: i.Description, Unit: i.Unit, AttributeFilter: allowed}
		if i.Name == "http_request_duration" {
			s.Aggregation = sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
			}
		}
		return s, true
	}
	return []sdkmetric.Option{sdkmetric.WithView(view)}
}

func keys(ss []string) []attribute.Key {
	out := make([]attribute.Key, len(ss))
	for i, s := range ss {
		out[i] = attribute.Key(s)
	}
	return out
}

// Metrics holds the OBS-REQ-004 instruments. Prometheus names after the
// collector's suffixing: http_request_duration_seconds, jobs_queue_depth,
// jobs_queue_age_seconds, jobs_attempts_total, jobs_failed_total,
// provider_errors_total, tokens_total, estimated_cost_usd_total,
// rerank_fallbacks_total, ingest_failures_total, guard_verdicts_total,
// uncited_answers_total.
type Metrics struct {
	meter          metric.Meter
	httpDuration   metric.Float64Histogram
	jobAttempts    metric.Int64Counter
	jobFailed      metric.Int64Counter
	providerErrors metric.Int64Counter
	tokens         metric.Int64Counter
	cost           metric.Float64Counter
	rerankFallback metric.Int64Counter
	ingestFailures metric.Int64Counter
	guardVerdicts  metric.Int64Counter
	uncited        metric.Int64Counter
	sweeper        metric.Int64Counter
}

// NewMetrics creates the instruments on mp.
func NewMetrics(mp metric.MeterProvider) *Metrics {
	m := &Metrics{meter: mp.Meter("github.com/kavish924/multi-tenant-rag")}
	m.httpDuration, _ = m.meter.Float64Histogram("http_request_duration", metric.WithUnit("s"),
		metric.WithDescription("HTTP request latency by route and status."))
	m.jobAttempts, _ = m.meter.Int64Counter("jobs_attempts", metric.WithDescription("Job attempts started, by kind."))
	m.jobFailed, _ = m.meter.Int64Counter("jobs_failed", metric.WithDescription("Failed job attempts, by kind and outcome (retry|dead)."))
	m.providerErrors, _ = m.meter.Int64Counter("provider_errors", metric.WithDescription("Provider call errors, by provider and kind."))
	m.tokens, _ = m.meter.Int64Counter("tokens", metric.WithDescription("Provider tokens by tenant, kind and direction."))
	m.cost, _ = m.meter.Float64Counter("estimated_cost_usd", metric.WithDescription("Estimated provider cost in USD by tenant."))
	m.rerankFallback, _ = m.meter.Int64Counter("rerank_fallbacks", metric.WithDescription("Rerank stage fallbacks to fused order, by reason."))
	m.ingestFailures, _ = m.meter.Int64Counter("ingest_failures", metric.WithDescription("Ingestion failures by reason code (parser failure reasons)."))
	m.guardVerdicts, _ = m.meter.Int64Counter("guard_verdicts", metric.WithDescription("Guard Block/Redact verdicts by stage (spec 012)."))
	m.uncited, _ = m.meter.Int64Counter("uncited_answers", metric.WithDescription("Answers with sources but no citation (soft signal, spec 012)."))
	m.sweeper, _ = m.meter.Int64Counter("sweeper_removed", metric.WithDescription("Rows and objects removed by the retention sweeper, by kind (spec 013)."))
	return m
}

var (
	defaultOnce sync.Once
	defaultM    *Metrics
)

// M returns the process-wide instruments on the global meter provider
// (which forwards to the provider Setup installs).
func M() *Metrics {
	defaultOnce.Do(func() { defaultM = NewMetrics(otel.GetMeterProvider()) })
	return defaultM
}

// HTTPRequest records one request's latency.
func (m *Metrics) HTTPRequest(ctx context.Context, method, route string, status int, d time.Duration) {
	m.httpDuration.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String("method", method), attribute.String("route", route), attribute.Int("status", status)))
}

// JobAttempt counts a claimed job.
func (m *Metrics) JobAttempt(ctx context.Context, kind string) {
	m.jobAttempts.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
}

// JobFailed counts a failed attempt; dead reports dead-lettering (OBS-AC-005).
func (m *Metrics) JobFailed(ctx context.Context, kind string, dead bool) {
	outcome := "retry"
	if dead {
		outcome = "dead"
	}
	m.jobFailed.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind), attribute.String("outcome", outcome)))
}

// ProviderError counts a failed provider call; kind is embed, generate or rerank.
func (m *Metrics) ProviderError(ctx context.Context, provider, kind string) {
	m.providerErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("provider", provider), attribute.String("kind", kind)))
}

// RerankFallback counts a rerank fallback (spec 009 OnFallback hook).
func (m *Metrics) RerankFallback(ctx context.Context, reason string) {
	m.rerankFallback.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// IngestFailure counts an ingestion failure by its reason code (005).
func (m *Metrics) IngestFailure(ctx context.Context, reason string) {
	m.ingestFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// GuardVerdict counts a Block or Redact (GRD-REQ-010).
func (m *Metrics) GuardVerdict(ctx context.Context, tenantID uuid.UUID, stage, verdict string) {
	m.guardVerdicts.Add(ctx, 1, metric.WithAttributes(attribute.String("tenant_id", tenantID.String()),
		attribute.String("stage", stage), attribute.String("verdict", verdict)))
}

// UncitedAnswer counts an answer that cited nothing although sources existed.
func (m *Metrics) UncitedAnswer(ctx context.Context, tenantID uuid.UUID) {
	m.uncited.Add(ctx, 1, metric.WithAttributes(attribute.String("tenant_id", tenantID.String())))
}

// SweeperRemoved reports one sweep's removals (DEL-AC-008).
func (m *Metrics) SweeperRemoved(ctx context.Context, kind string, n int) {
	m.sweeper.Add(ctx, int64(n), metric.WithAttributes(attribute.String("kind", kind)))
}

// Usage feeds tokens_total and estimated_cost_usd_total from one usage row.
func (m *Metrics) Usage(ctx context.Context, tenantID uuid.UUID, kind, model string, in, out int) {
	t := attribute.String("tenant_id", tenantID.String())
	k := attribute.String("kind", kind)
	if in > 0 {
		m.tokens.Add(ctx, int64(in), metric.WithAttributes(t, k, attribute.String("direction", "input")))
	}
	if out > 0 {
		m.tokens.Add(ctx, int64(out), metric.WithAttributes(t, k, attribute.String("direction", "output")))
	}
	if usd := EstimateCost(model, in, out); usd > 0 {
		m.cost.Add(ctx, usd, metric.WithAttributes(t))
	}
}

// QueueStat is one job kind's queue state.
type QueueStat struct {
	Kind       string
	Depth      int64   // runnable or running jobs
	OldestAgeS float64 // age of the oldest runnable job, seconds
}

// RegisterQueueGauges reports jobs_queue_depth and jobs_queue_age_seconds
// at each collection by calling stats (the worker passes jobs.Queue.Stats).
func (m *Metrics) RegisterQueueGauges(stats func(context.Context) ([]QueueStat, error)) error {
	depth, err := m.meter.Int64ObservableGauge("jobs_queue_depth", metric.WithDescription("Queued plus running jobs, by kind."))
	if err != nil {
		return err
	}
	age, err := m.meter.Float64ObservableGauge("jobs_queue_age", metric.WithUnit("s"),
		metric.WithDescription("Age of the oldest runnable job, by kind."))
	if err != nil {
		return err
	}
	_, err = m.meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		ss, err := stats(ctx)
		if err != nil {
			return err
		}
		for _, s := range ss {
			kind := metric.WithAttributes(attribute.String("kind", s.Kind))
			o.ObserveInt64(depth, s.Depth, kind)
			o.ObserveFloat64(age, s.OldestAgeS, kind)
		}
		return nil
	}, depth, age)
	return err
}

// Price is a model's estimated USD price per 1,000 tokens.
type Price struct{ InputPer1K, OutputPer1K float64 }

// Prices are list-price estimates for cost attribution (not billing).
// Verify against current Vertex AI pricing before relying on them; fakes
// cost nothing.
var Prices = map[string]Price{
	"gemini-2.5-flash":     {InputPer1K: 0.0003, OutputPer1K: 0.0025},
	"gemini-2.5-pro":       {InputPer1K: 0.00125, OutputPer1K: 0.01},
	"text-embedding-005":   {InputPer1K: 0.0001},
	"gemini-embedding-001": {InputPer1K: 0.00015},
}

// EstimateCost prices a call; unknown models cost 0.
func EstimateCost(model string, in, out int) float64 {
	p, ok := Prices[model]
	if !ok {
		return 0
	}
	return float64(in)/1000*p.InputPer1K + float64(out)/1000*p.OutputPer1K
}
