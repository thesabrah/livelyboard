package telemetry

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	_ "modernc.org/sqlite"
)

func TestSetupDisabledIsNoop(t *testing.T) {
	shutdown, err := Setup(context.Background(), "", nil)
	if err != nil || shutdown(context.Background()) != nil {
		t.Fatal("empty endpoint must be a no-op")
	}
}

// The span/metric route must be the ServeMux pattern, not the raw path (no cardinality explosion).
func TestOtelhttpUsesRoutePattern(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	rd := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(rd))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /teams/{id}/rank", func(http.ResponseWriter, *http.Request) {})
	h := otelhttp.NewHandler(mux, "http", otelhttp.WithTracerProvider(tp), otelhttp.WithMeterProvider(mp))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/teams/42/rank", nil))

	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			found = found || m.Name == "http.server.request.duration"
		}
	}
	if !found {
		t.Fatal("http.server.request.duration not recorded")
	}
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	for _, a := range spans[0].Attributes() {
		if a.Key == "http.route" && a.Value.AsString() != "/teams/{id}/rank" {
			t.Fatalf("route = %q", a.Value.AsString())
		}
	}
}

// Exporters connect lazily, so Setup succeeds without a running Collector.
func TestSetupWithEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:1")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	shutdown, err := Setup(context.Background(), "localhost:1", map[string]*sql.DB{"write": openDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = shutdown(ctx) // the final flush fails: nothing is listening
}

func TestPoolGauges(t *testing.T) {
	rd := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(rd))
	pool := openDB(t)
	if err := pool.Ping(); err != nil { // opens one connection
		t.Fatal(err)
	}
	if err := poolGauges(mp.Meter("test"), map[string]*sql.DB{"write": pool}); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if g, ok := m.Data.(metricdata.Gauge[int64]); ok && m.Name == "db.pool.open" {
				if p, _ := g.DataPoints[0].Attributes.Value("pool"); p.AsString() != "write" || g.DataPoints[0].Value != 1 {
					t.Fatalf("db.pool.open = %+v", g.DataPoints)
				}
				return
			}
		}
	}
	t.Fatal("db.pool.open not reported")
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
