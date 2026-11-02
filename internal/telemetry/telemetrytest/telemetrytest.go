// Package telemetrytest records blobfork's spans and counters in memory, for
// tests.
package telemetrytest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Recorder holds what was recorded since Start.
type Recorder struct {
	spans   *tracetest.SpanRecorder
	metrics *sdkmetric.ManualReader
}

// Start records the process's spans and counters from now on. Make handlers
// to trace (telemetry.Handler) after it.
func Start() *Recorder {
	r := &Recorder{spans: tracetest.NewSpanRecorder(), metrics: sdkmetric.NewManualReader()}

	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(r.spans)))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(r.metrics)))

	return r
}

// Span is the last ended span with a name, as a map of its attributes; nil
// if there's none.
func (r *Recorder) Span(name string) map[string]string {
	ended := r.spans.Ended()

	for i := len(ended) - 1; i >= 0; i-- {
		if ended[i].Name() != name {
			continue
		}

		attrs := map[string]string{}
		for _, kv := range ended[i].Attributes() {
			attrs[string(kv.Key)] = kv.Value.Emit()
		}

		return attrs
	}

	return nil
}

// Count is a counter's total for a container or bucket, a gauge's value,
// or how many a histogram timed.
func (r *Recorder) Count(t *testing.T, name, container string) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics

	if err := r.metrics.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}

	var total int64

	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}

			mine := func(attrs attribute.Set) bool {
				value, ok := attrs.Value(attribute.Key("blobfork.container"))

				return ok && value.AsString() == container
			}

			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, point := range data.DataPoints {
					if mine(point.Attributes) {
						total += point.Value
					}
				}

			case metricdata.Gauge[int64]:
				for _, point := range data.DataPoints {
					if mine(point.Attributes) {
						total += point.Value
					}
				}

			// A histogram's count: how many it timed.
			case metricdata.Histogram[float64]:
				for _, point := range data.DataPoints {
					if mine(point.Attributes) {
						total += int64(point.Count)
					}
				}
			}
		}
	}

	return total
}
