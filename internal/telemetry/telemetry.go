// Package telemetry is blobfork's OpenTelemetry: traces and metrics over
// OTLP/HTTP, on only when an OTLP endpoint is set (OTEL_EXPORTER_OTLP_ENDPOINT,
// or _TRACES_ENDPOINT, _METRICS_ENDPOINT), and the counters both fronts add
// to. Without an endpoint, the spans and counters go nowhere.
package telemetry

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// What answered a request, on its span as blobfork.answered_by.
const (
	AnsweredByFork  = "fork"
	AnsweredByLower = "lower"
	AnsweredByMerge = "merge"
)

var meter = otel.Meter("github.com/stagdhq/blobfork")

// The counters, each with the front (blobfork.front) and the container or
// bucket (blobfork.container), for a dashboard of a fork.
var (
	LowerReads  = counter("blobfork.lower.reads", "{read}", "Reads the fork answered from the real container or bucket.")
	LowerBytes  = counter("blobfork.lower.bytes", "By", "Bytes read from the real container or bucket, for the app or a copy-up: what the fork costs it in egress.")
	CopyUps     = counter("blobfork.copy_ups", "{file}", "Files copied from the real container or bucket into the fork.")
	CopyUpBytes = counter("blobfork.copy_ups.bytes", "By", "Bytes copied from the real container or bucket into the fork.")
	Hidden      = counter("blobfork.deletes.hidden", "{file}", "Delete markers written, hiding a file of the real container or bucket.")
	MergedPages = counter("blobfork.listings.merged", "{page}", "Listing pages merged from both layers.")

	// Requests are the requests blobfork served, also by operation
	// (blobfork.operation: read, list, …), what answered them
	// (blobfork.answered_by) and how they ended (blobfork.outcome: ok,
	// client_error, server_error).
	Requests        = counter("blobfork.requests", "{request}", "Requests blobfork served.")
	RequestDuration = histogram("blobfork.request.duration", "How long blobfork took to answer a request.")

	// LowerDuration is how long the real container or bucket takes to
	// answer, by operation (read, head, list).
	LowerDuration = histogram("blobfork.lower.duration", "How long the real container or bucket took to answer.")
)

func counter(name, unit, description string) metric.Int64Counter {
	c, _ := meter.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(description))

	return c
}

func histogram(name, description string) metric.Float64Histogram {
	h, _ := meter.Float64Histogram(name, metric.WithUnit("s"), metric.WithDescription(description))

	return h
}

// ObserveMarkers reports, at each collection, how many files of the real
// containers or buckets each fork hides (blobfork.deletes.markers): each
// calls report for every container or bucket.
func ObserveMarkers(front string, each func(report func(container string, n int))) {
	gauge, _ := meter.Int64ObservableGauge("blobfork.deletes.markers",
		metric.WithUnit("{file}"),
		metric.WithDescription("Files of the real container or bucket the fork hides."),
	)

	meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		each(func(container string, n int) {
			o.ObserveInt64(gauge, int64(n), Attrs(front, container))
		})

		return nil
	}, gauge)
}

// Lower times a request to the real container or bucket.
func Lower(ctx context.Context, front, container, operation string, started time.Time) {
	LowerDuration.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(
		attribute.String("blobfork.front", front),
		attribute.String("blobfork.container", container),
		attribute.String("blobfork.operation", operation),
	))
}

// Attrs are a counter's attributes: the front and its container or bucket.
func Attrs(front, container string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("blobfork.front", front), attribute.String("blobfork.container", container))
}

// request is what blobfork knows of a request it serves, for its span and
// its metrics.
type request struct {
	span      trace.Span
	front     string
	operation string
	container string
	answered  string
	started   time.Time
}

type requestKey struct{}

// Name names the request's span by what blobfork does with it, says which
// container or bucket it's for, and keeps it in the context for Answered and
// Finish: the requests blobfork makes for it have spans of their own.
func Name(ctx context.Context, front, operation, container string) context.Context {
	span := trace.SpanFromContext(ctx)

	span.SetName(operation)
	span.SetAttributes(attribute.String("blobfork.front", front))

	if container != "" {
		span.SetAttributes(attribute.String("blobfork.container", container))
	}

	r := &request{span: span, front: front, operation: operation, container: container, started: time.Now()}

	return context.WithValue(ctx, requestKey{}, r)
}

// Answered says what answered the request.
func Answered(ctx context.Context, by string) {
	r, ok := ctx.Value(requestKey{}).(*request)
	if !ok {
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("blobfork.answered_by", by))

		return
	}

	r.answered = by
	r.span.SetAttributes(attribute.String("blobfork.answered_by", by))
}

// Finish counts and times the request, once answered with status.
func Finish(ctx context.Context, status int) {
	r, ok := ctx.Value(requestKey{}).(*request)
	if !ok {
		return
	}

	outcome := "ok"

	switch {
	case status >= 500:
		outcome = "server_error"

	case status >= 400:
		outcome = "client_error"
	}

	attrs := metric.WithAttributes(
		attribute.String("blobfork.front", r.front),
		attribute.String("blobfork.container", r.container),
		attribute.String("blobfork.operation", operationKind(r.operation)),
		attribute.String("blobfork.answered_by", r.answered),
		attribute.String("blobfork.outcome", outcome),
	)

	Requests.Add(ctx, 1, attrs)
	RequestDuration.Record(ctx, time.Since(r.started).Seconds(), attrs)
}

// operationKind keeps a metric's operations few: "pass PUT" and the like
// are "pass".
func operationKind(operation string) string {
	kind, _, _ := strings.Cut(operation, " ")
	if kind == "list" {
		return operation
	}

	return kind
}

// StatusWriter keeps the status a handler answers with, for Finish.
type StatusWriter struct {
	http.ResponseWriter
	Status int
}

func (w *StatusWriter) WriteHeader(status int) {
	if w.Status == 0 {
		w.Status = status
	}

	w.ResponseWriter.WriteHeader(status)
}

func (w *StatusWriter) Write(b []byte) (int, error) {
	if w.Status == 0 {
		w.Status = http.StatusOK
	}

	return w.ResponseWriter.Write(b)
}

// Unwrap lets the proxy flush through it.
func (w *StatusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Client is an HTTP client whose requests are spans, for the emulator and
// the real containers and buckets.
var Client = &http.Client{Transport: Transport(http.DefaultTransport)}

// Transport makes a transport's requests spans, named for the host they go
// to.
func Transport(base http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(base, otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
		return r.Method + " " + r.URL.Host
	}))
}

// Handler makes the requests an http.Handler serves spans, joining the
// app's trace if it sent one; health checks (/_blobfork/…) are left out.
func Handler(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "blobfork", otelhttp.WithFilter(func(r *http.Request) bool {
		return !strings.HasPrefix(r.URL.Path, "/_blobfork/")
	}))
}

// Enabled says whether an OTLP endpoint is set, and the SDK not turned off.
func Enabled() bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}

	for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
		if os.Getenv(key) != "" {
			return true
		}
	}

	return false
}

// Setup turns telemetry on when Enabled, exporting with the standard OTEL_*
// variables (endpoint, headers, service name, resource attributes). The
// function it returns flushes and stops it.
func Setup(ctx context.Context, version string) (func(context.Context) error, error) {
	if !Enabled() {
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("blobfork"), semconv.ServiceVersion(version)),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, err
	}

	traces, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}

	metrics, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}

	tracer := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traces), sdktrace.WithResource(res))
	meters := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metrics)), sdkmetric.WithResource(res))

	otel.SetTracerProvider(tracer)
	otel.SetMeterProvider(meters)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	return func(ctx context.Context) error {
		return errors.Join(tracer.Shutdown(ctx), meters.Shutdown(ctx))
	}, nil
}
