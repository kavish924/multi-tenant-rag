// Package observability wires OpenTelemetry traces and metrics, request
// ids, and Cloud Logging-compatible structured logs (spec 010).
//
// Telemetry never blocks requests (OBS-AC-006): spans and metrics are
// exported asynchronously in batches with short exporter timeouts, and an
// unreachable collector only drops telemetry.
package observability

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Config selects where telemetry goes.
type Config struct {
	Service string
	Version string
	// Endpoint is the OTLP gRPC collector address (OTEL_EXPORTER_OTLP_ENDPOINT,
	// for example "localhost:4317" for the Cloud Run sidecar). Empty disables
	// export: spans and metrics are still created (for request ids, log
	// correlation and tests) but go nowhere.
	Endpoint string
	// SampleRatio is the head-sampling ratio for new traces (parent-based:
	// a sampled caller keeps its decision). Default 1.0; production 0.1.
	SampleRatio float64
	// ProjectID formats the Cloud Logging trace field
	// ("projects/<id>/traces/<trace>"); GOOGLE_CLOUD_PROJECT.
	ProjectID string
	// ExportTimeout bounds each export call (default 2s).
	ExportTimeout time.Duration
	// MetricInterval is how often metrics are pushed (default 30s).
	MetricInterval time.Duration
}

// ConfigFromEnv reads the standard OTel variables plus GOOGLE_CLOUD_PROJECT.
func ConfigFromEnv(service string) (Config, error) {
	c := Config{
		Service:     service,
		Version:     os.Getenv("SERVICE_VERSION"),
		Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		SampleRatio: 1,
		ProjectID:   os.Getenv("GOOGLE_CLOUD_PROJECT"),
	}
	if v := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || r < 0 || r > 1 {
			return Config{}, errors.New("observability: OTEL_TRACES_SAMPLER_ARG must be in [0, 1]")
		}
		c.SampleRatio = r
	}
	return c, nil
}

var projectID string

// Setup installs the global tracer and meter providers and the W3C
// propagator. The returned shutdown flushes what it can within ctx.
func Setup(ctx context.Context, c Config) (shutdown func(context.Context) error, err error) {
	projectID = c.ProjectID
	if c.ExportTimeout <= 0 {
		c.ExportTimeout = 2 * time.Second
	}
	if c.MetricInterval <= 0 {
		c.MetricInterval = 30 * time.Second
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", c.Service),
		attribute.String("service.version", c.Version),
	))
	if err != nil {
		return nil, fmt.Errorf("observability: resource: %w", err)
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(c.SampleRatio))),
	}
	mpOpts := append([]sdkmetric.Option{sdkmetric.WithResource(res)}, MeterOptions()...)

	if c.Endpoint != "" {
		// The sidecar listens on localhost without TLS; exports are async
		// and time-bounded, so a dead collector drops data and nothing else.
		te, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(c.Endpoint), otlptracegrpc.WithInsecure(),
			otlptracegrpc.WithTimeout(c.ExportTimeout), otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: false}))
		if err != nil {
			return nil, fmt.Errorf("observability: trace exporter: %w", err)
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(te,
			sdktrace.WithMaxQueueSize(4096), sdktrace.WithExportTimeout(c.ExportTimeout)))

		me, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpoint(c.Endpoint), otlpmetricgrpc.WithInsecure(),
			otlpmetricgrpc.WithTimeout(c.ExportTimeout), otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{Enabled: false}))
		if err != nil {
			return nil, fmt.Errorf("observability: metric exporter: %w", err)
		}
		mpOpts = append(mpOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me,
			sdkmetric.WithInterval(c.MetricInterval), sdkmetric.WithTimeout(c.ExportTimeout))))
	}

	tp := sdktrace.NewTracerProvider(tpOpts...)
	mp := sdkmetric.NewMeterProvider(mpOpts...)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	return func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx))
	}, nil
}
