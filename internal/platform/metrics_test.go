package platform

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestMetricsEmitsTheThreeBusinessMetrics(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	m, err := NewMetrics()
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	ctx := context.Background()
	m.RecordBreakerTransition("partner-flaky", "closed", "open", 1)
	m.CacheHit(ctx, "corretora-a")
	m.CacheMiss(ctx, "corretora-a")
	m.PartnerLatency(ctx, "partner-slow", 1500*time.Millisecond, false)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			seen[md.Name] = true
		}
	}

	for _, name := range []string{
		MetricBreakerTransitions,
		MetricBreakerState,
		MetricCacheEvents,
		MetricPartnerLatency,
	} {
		if !seen[name] {
			t.Errorf("metric %q was not emitted", name)
		}
	}
}

func TestTraceMarkingsAddSpanEvents(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	m, err := NewMetrics()
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	ctx, span := tp.Tracer("test").Start(context.Background(), "POST /quotes")
	m.ShortCircuited(ctx, "partner-flaky")
	m.ServedFromCache(ctx, true)
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("%d spans ended, want 1", len(ended))
	}

	events := map[string]bool{}
	for _, e := range ended[0].Events() {
		events[e.Name] = true
	}
	if !events["breaker.short_circuited"] {
		t.Error("short-circuit trace marking missing")
	}
	if !events["cache.served"] {
		t.Error("cache-served trace marking missing")
	}
}

// TestNoPersonalDataInAttributeKeys guards the reproves-alone rule at the code
// level: the attribute keys this package emits must not include cpf, plate or
// quote_id.
func TestNoPersonalDataInAttributeKeys(t *testing.T) {
	forbidden := []string{"cpf", "document", "plate", "quote_id"}
	keys := []string{attrPartner, attrFrom, attrTo, attrTenant, attrResult, attrOutcome}
	for _, k := range keys {
		for _, bad := range forbidden {
			if k == bad {
				t.Errorf("forbidden attribute key %q is emitted", k)
			}
		}
	}
}
