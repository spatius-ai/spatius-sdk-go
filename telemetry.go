package spatiussdkgo

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Process-wide unauthenticated OpenTelemetry metrics and traces for the SDK.
//
// This file intentionally does not register global OpenTelemetry providers. An
// application embedding the SDK may have its own providers, and this private
// provider must remain independently configurable and flushable.
//
// Only metrics and traces are exported. There is no OpenTelemetry logs provider
// or log exporter in the SDK.

const (
	// DefaultTelemetryEndpoint is the built-in OTLP base endpoint. The SDK derives
	// the /v1/metrics and /v1/traces signal endpoints from it.
	DefaultTelemetryEndpoint = "https://t.spatialwalk.top"

	telemetryExportInterval = 10 * time.Second
	telemetryExportTimeout  = 5 * time.Second
)

// A non-empty header prevents the OTLP exporters from importing credentials from
// OTEL_EXPORTER_OTLP_HEADERS. This SDK's endpoint is intentionally unauthenticated.
var telemetryExportHeaders = map[string]string{"User-Agent": "spatius-go-sdk"}

var telemetryState = struct {
	sync.Mutex
	endpoint                string
	resourceAppID           string
	resourceRegion          string
	initializationAttempted bool
	tracerProvider          *sdktrace.TracerProvider
	meterProvider           *sdkmetric.MeterProvider
	tracer                  trace.Tracer
	meter                   metric.Meter
	histograms              map[string]metric.Float64Histogram
}{
	endpoint:   DefaultTelemetryEndpoint,
	histograms: map[string]metric.Float64Histogram{},
}

var traceContextPropagator = propagation.TraceContext{}

// sdkVersion returns the module version reported in telemetry resource metadata
// and bootstrap requests.
func sdkVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Path == "github.com/spatius-ai/spatius-sdk-go" && info.Main.Version != "" {
			return info.Main.Version
		}
		for _, dep := range info.Deps {
			if dep.Path == "github.com/spatius-ai/spatius-sdk-go" && dep.Version != "" {
				return dep.Version
			}
		}
	}
	return "0+unknown"
}

func normalizeTelemetryEndpoint(endpoint string) (string, error) {
	value := strings.TrimSpace(endpoint)
	if value == "" {
		return "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("telemetry endpoint must be an absolute http:// or https:// URL")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("telemetry endpoint must not contain a query or fragment")
	}
	return strings.TrimRight(value, "/"), nil
}

// setResourceContext sets process-wide resource fields before providers are
// initialized.
//
// OTel resources are immutable. The first session that initializes telemetry
// establishes these values for the process; later sessions still carry their
// own app/region values on spans and metric attributes.
func setResourceContext(appID, region string) {
	telemetryState.Lock()
	defer telemetryState.Unlock()
	if telemetryState.tracerProvider != nil || telemetryState.meterProvider != nil {
		return
	}
	telemetryState.resourceAppID = appID
	telemetryState.resourceRegion = region
}

// ConfigureTelemetry configures the process-wide OTLP base endpoint.
//
// An empty endpoint disables both metrics and traces. Pass
// DefaultTelemetryEndpoint to restore the built-in endpoint.
//
// It returns an error when the endpoint is not an absolute HTTP(S) URL, or when
// a different endpoint is configured after providers have already been
// initialized. Configure before creating/using a session, or call
// ShutdownTelemetry first.
func ConfigureTelemetry(endpoint string) error {
	normalized, err := normalizeTelemetryEndpoint(endpoint)
	if err != nil {
		return err
	}
	telemetryState.Lock()
	defer telemetryState.Unlock()
	if (telemetryState.tracerProvider != nil || telemetryState.meterProvider != nil) && normalized != telemetryState.endpoint {
		return fmt.Errorf("telemetry is already initialized; call ShutdownTelemetry before changing its endpoint")
	}
	telemetryState.endpoint = normalized
	return nil
}

// telemetryEnabled reports whether telemetry has a non-empty configured endpoint.
func telemetryEnabled() bool {
	telemetryState.Lock()
	defer telemetryState.Unlock()
	return telemetryState.endpoint != ""
}

func telemetrySignalEndpoint(signal string) string {
	return telemetryState.endpoint + "/v1/" + signal
}

func ensureTelemetryInitialized() {
	telemetryState.Lock()
	defer telemetryState.Unlock()
	if telemetryState.endpoint == "" || telemetryState.initializationAttempted {
		return
	}
	telemetryState.initializationAttempted = true

	version := sdkVersion()
	res, err := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceName("spatius-go"),
			attribute.String("sdk.platform", "go"),
			attribute.String("sdk.package", "spatius-go"),
			attribute.String("sdk.version", version),
			attribute.String("app_id", telemetryState.resourceAppID),
			attribute.String("region", telemetryState.resourceRegion),
		),
	)
	if err != nil {
		res = resource.Default()
	}

	ctx, cancel := context.WithTimeout(context.Background(), telemetryExportTimeout)
	defer cancel()

	spanExporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(telemetrySignalEndpoint("traces")),
		otlptracehttp.WithHeaders(telemetryExportHeaders),
		otlptracehttp.WithTimeout(telemetryExportTimeout),
	)
	if err != nil {
		log.Printf("spatiussdkgo: failed to initialize OpenTelemetry traces: %v", err)
	} else {
		provider := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(spanExporter)),
		)
		telemetryState.tracerProvider = provider
		telemetryState.tracer = provider.Tracer("spatius", trace.WithInstrumentationVersion(version))
	}

	metricExporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(telemetrySignalEndpoint("metrics")),
		otlpmetrichttp.WithHeaders(telemetryExportHeaders),
		otlpmetrichttp.WithTimeout(telemetryExportTimeout),
		otlpmetrichttp.WithTemporalitySelector(func(kind sdkmetric.InstrumentKind) metricdata.Temporality {
			if kind == sdkmetric.InstrumentKindHistogram {
				return metricdata.DeltaTemporality
			}
			return sdkmetric.DefaultTemporalitySelector(kind)
		}),
	)
	if err != nil {
		log.Printf("spatiussdkgo: failed to initialize OpenTelemetry metrics: %v", err)
	} else {
		reader := sdkmetric.NewPeriodicReader(metricExporter,
			sdkmetric.WithInterval(telemetryExportInterval),
			sdkmetric.WithTimeout(telemetryExportTimeout),
		)
		provider := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(reader),
			sdkmetric.WithView(sdkmetric.NewView(
				sdkmetric.Instrument{Name: "http.client.request.duration"},
				sdkmetric.Stream{
					Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
						Boundaries: []float64{100, 200, 500, 1000, 2000, 3000, 4000, 5000},
					},
				},
			)),
		)
		telemetryState.meterProvider = provider
		telemetryState.meter = provider.Meter("spatius-go", metric.WithInstrumentationVersion(version))
	}
}

func telemetryAttributes(attrs map[string]any) []attribute.KeyValue {
	kvs := make([]attribute.KeyValue, 0, len(attrs))
	for key, value := range attrs {
		switch v := value.(type) {
		case string:
			kvs = append(kvs, attribute.String(key, v))
		case bool:
			kvs = append(kvs, attribute.Bool(key, v))
		case int:
			kvs = append(kvs, attribute.Int(key, v))
		case int64:
			kvs = append(kvs, attribute.Int64(key, v))
		case float64:
			kvs = append(kvs, attribute.Float64(key, v))
		default:
			kvs = append(kvs, attribute.String(key, fmt.Sprint(v)))
		}
	}
	return kvs
}

// startSpan starts a span, returning nil when telemetry is disabled or
// unavailable. Telemetry failures never affect SDK behavior.
func startSpan(name string, attrs map[string]any) (span trace.Span) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("spatiussdkgo: failed to start OpenTelemetry span %s: %v", name, recovered)
			span = nil
		}
	}()

	ensureTelemetryInitialized()
	telemetryState.Lock()
	tracer := telemetryState.tracer
	telemetryState.Unlock()
	if tracer == nil {
		return nil
	}
	_, span = tracer.Start(context.Background(), name, trace.WithAttributes(telemetryAttributes(attrs)...))
	return span
}

// finishSpan sets final span data and ends it, isolating all telemetry failures.
func finishSpan(span trace.Span, attrs map[string]any, err error) {
	if span == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("spatiussdkgo: failed to finish OpenTelemetry span: %v", recovered)
		}
	}()
	if len(attrs) > 0 {
		span.SetAttributes(telemetryAttributes(attrs)...)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

func addSpanEvent(span trace.Span, name string, attrs map[string]any) {
	if span == nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("spatiussdkgo: failed to add OpenTelemetry span event %s: %v", name, recovered)
		}
	}()
	span.AddEvent(name, trace.WithAttributes(telemetryAttributes(attrs)...))
}

// injectTraceContext returns W3C trace context for a first audio message.
func injectTraceContext(span trace.Span) (carrier map[string]string) {
	if span == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("spatiussdkgo: failed to inject OpenTelemetry trace context: %v", recovered)
			carrier = nil
		}
	}()
	prop := propagation.MapCarrier{}
	traceContextPropagator.Inject(trace.ContextWithSpan(context.Background(), span), prop)
	return map[string]string(prop)
}

// recordMetric records a histogram observation without affecting SDK behavior.
func recordMetric(name string, value float64, attrs map[string]any) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("spatiussdkgo: failed to record OpenTelemetry metric %s: %v", name, recovered)
		}
	}()

	ensureTelemetryInitialized()
	telemetryState.Lock()
	meter := telemetryState.meter
	histogram := telemetryState.histograms[name]
	if meter != nil && histogram == nil {
		created, err := meter.Float64Histogram(name)
		if err != nil {
			telemetryState.Unlock()
			log.Printf("spatiussdkgo: failed to create OpenTelemetry metric %s: %v", name, err)
			return
		}
		histogram = created
		telemetryState.histograms[name] = histogram
	}
	telemetryState.Unlock()

	if histogram == nil {
		return
	}
	histogram.Record(context.Background(), value, metric.WithAttributes(telemetryAttributes(attrs)...))
}

// recordHTTPClientDuration records one HTTP client request observation. A
// statusCode of 0 marks a transport error.
func recordHTTPClientDuration(operation, method string, durationMS float64, statusCode int, serverAddress string) {
	attrs := map[string]any{
		"http.request.method": method,
		"operation":           operation,
	}
	if attrs["operation"] == "" {
		attrs["operation"] = "_OTHER"
	}
	if serverAddress != "" {
		attrs["server.address"] = serverAddress
	}
	if statusCode > 0 {
		attrs["http.response.status_code"] = statusCode
	} else {
		attrs["error.type"] = "transport_error"
	}
	recordMetric("http.client.request.duration", durationMS, attrs)
}

// ForceFlushTelemetry flushes both providers without shutting them down.
func ForceFlushTelemetry() {
	telemetryState.Lock()
	tracerProvider := telemetryState.tracerProvider
	meterProvider := telemetryState.meterProvider
	telemetryState.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), telemetryExportTimeout)
	defer cancel()
	flush := func(fn func(context.Context) error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("spatiussdkgo: failed to flush OpenTelemetry provider: %v", recovered)
			}
		}()
		_ = fn(ctx)
	}
	if tracerProvider != nil {
		flush(tracerProvider.ForceFlush)
	}
	if meterProvider != nil {
		flush(meterProvider.ForceFlush)
	}
}

// ShutdownTelemetry flushes and shuts down the process-wide providers.
//
// The batch processors export asynchronously. Applications that exit
// immediately after a session should call ShutdownTelemetry so pending
// metrics and traces are flushed.
func ShutdownTelemetry() {
	telemetryState.Lock()
	tracerProvider := telemetryState.tracerProvider
	meterProvider := telemetryState.meterProvider
	telemetryState.tracerProvider = nil
	telemetryState.meterProvider = nil
	telemetryState.tracer = nil
	telemetryState.meter = nil
	telemetryState.histograms = map[string]metric.Float64Histogram{}
	telemetryState.resourceAppID = ""
	telemetryState.resourceRegion = ""
	telemetryState.initializationAttempted = false
	telemetryState.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), telemetryExportTimeout)
	defer cancel()
	shutdown := func(fn func(context.Context) error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("spatiussdkgo: failed to shut down OpenTelemetry provider: %v", recovered)
			}
		}()
		_ = fn(ctx)
	}
	if tracerProvider != nil {
		shutdown(tracerProvider.Shutdown)
	}
	if meterProvider != nil {
		shutdown(meterProvider.Shutdown)
	}
}
