// Package telemetry wires OpenTelemetry metrics and traces, pushed over OTLP/gRPC to a Collector.
package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Setup installs global meter/tracer providers. It is opt-in: when endpoint is empty it does
// nothing, so the global providers stay no-ops and local runs/tests need no Collector.
// Everything else is configured through the standard OTEL_* env vars (OTEL_EXPORTER_OTLP_INSECURE,
// OTEL_TRACES_SAMPLER, OTEL_TRACES_SAMPLER_ARG, OTEL_SERVICE_NAME ...).
// pools are exported as connection-pool gauges, keyed by name ("write", "read").
// The returned shutdown flushes pending data; call it after the HTTP server has drained.
func Setup(ctx context.Context, endpoint string, pools map[string]*sql.DB) (shutdown func(context.Context) error, err error) {
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("livelyboard")),
		resource.WithFromEnv(), // OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES override the default
	)
	if err != nil {
		return nil, err
	}

	te, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}
	// Batcher: spans are queued and sent in the background, never on the request path.
	// Sampler comes from OTEL_TRACES_SAMPLER (default: parent-based, always on).
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(te), sdktrace.WithResource(res))

	me, err := otlpmetricgrpc.New(ctx)
	if err != nil {
		return nil, errors.Join(err, tp.Shutdown(ctx))
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me, sdkmetric.WithInterval(10*time.Second))),
		sdkmetric.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	shutdown = func(ctx context.Context) error { return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx)) }
	if err := runtime.Start(); err != nil { // goroutines, GC, heap
		return nil, errors.Join(err, shutdown(ctx))
	}
	if err := poolGauges(mp.Meter("livelyboard/db"), pools); err != nil {
		return nil, errors.Join(err, shutdown(ctx))
	}
	return shutdown, nil
}

// poolGauges reports database/sql pool stats. "wait_count" and "wait_duration" growing on the
// write pool is the signal that the single SQLite writer has become the bottleneck.
func poolGauges(m metric.Meter, pools map[string]*sql.DB) error {
	open, err := m.Int64ObservableGauge("db.pool.open", metric.WithDescription("open connections"))
	if err != nil {
		return err
	}
	inUse, err := m.Int64ObservableGauge("db.pool.in_use", metric.WithDescription("connections in use"))
	if err != nil {
		return err
	}
	waits, err := m.Int64ObservableCounter("db.pool.wait_count", metric.WithDescription("times a caller had to wait for a connection"))
	if err != nil {
		return err
	}
	waitSecs, err := m.Float64ObservableCounter("db.pool.wait_duration", metric.WithUnit("s"), metric.WithDescription("total time spent waiting for a connection"))
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for name, p := range pools {
			s, a := p.Stats(), metric.WithAttributes(attribute.String("pool", name))
			o.ObserveInt64(open, int64(s.OpenConnections), a)
			o.ObserveInt64(inUse, int64(s.InUse), a)
			o.ObserveInt64(waits, s.WaitCount, a)
			o.ObserveFloat64(waitSecs, s.WaitDuration.Seconds(), a)
		}
		return nil
	}, open, inUse, waits, waitSecs)
	return err
}
