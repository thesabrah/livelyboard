package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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
