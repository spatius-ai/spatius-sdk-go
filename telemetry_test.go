package spatiussdkgo

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// TestMain disables telemetry for the whole test package so that session tests
// never export to the real telemetry endpoint. Telemetry tests re-enable it
// explicitly against local servers.
func TestMain(m *testing.M) {
	if err := ConfigureTelemetry(""); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// enableTestTelemetry points telemetry at the given base endpoint and restores
// the disabled state afterwards.
func enableTestTelemetry(t *testing.T, endpoint string) {
	t.Helper()
	ShutdownTelemetry()
	if err := ConfigureTelemetry(endpoint); err != nil {
		t.Fatalf("ConfigureTelemetry returned error: %v", err)
	}
	t.Cleanup(func() {
		ShutdownTelemetry()
		if err := ConfigureTelemetry(""); err != nil {
			t.Fatalf("failed to disable telemetry: %v", err)
		}
	})
}

func TestDefaultTelemetryEndpointAndSignalPaths(t *testing.T) {
	if DefaultTelemetryEndpoint != "https://t.spatialwalk.top" {
		t.Fatalf("unexpected default telemetry endpoint: %q", DefaultTelemetryEndpoint)
	}

	enableTestTelemetry(t, DefaultTelemetryEndpoint)
	telemetryState.Lock()
	metrics := telemetrySignalEndpoint("metrics")
	traces := telemetrySignalEndpoint("traces")
	telemetryState.Unlock()
	if metrics != "https://t.spatialwalk.top/v1/metrics" {
		t.Fatalf("unexpected metrics endpoint: %q", metrics)
	}
	if traces != "https://t.spatialwalk.top/v1/traces" {
		t.Fatalf("unexpected traces endpoint: %q", traces)
	}
}

func TestEmptyEndpointDisablesTelemetry(t *testing.T) {
	enableTestTelemetry(t, "")
	if telemetryEnabled() {
		t.Fatal("expected telemetry to be disabled")
	}
	if span := startSpan("disabled", nil); span != nil {
		t.Fatal("expected no span when telemetry is disabled")
	}
	// Recording must be a safe no-op.
	recordMetric("test.metric", 1, nil)
	recordHTTPClientDuration("/op", http.MethodPost, 1, 200, "example.com")
}

func TestInvalidEndpointIsRejected(t *testing.T) {
	if err := ConfigureTelemetry("collector.example.com"); err == nil {
		t.Fatal("expected error for endpoint without scheme")
	}
	if err := ConfigureTelemetry("https://collector.example.com?token=secret"); err == nil {
		t.Fatal("expected error for endpoint with query")
	}
	if telemetryEnabled() {
		t.Fatal("endpoint validation failure must not change the configured endpoint")
	}
}

func TestConfigureTelemetryRejectsChangeAfterInitialization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	enableTestTelemetry(t, server.URL)
	setResourceContext("app-1", "eu-central")
	recordMetric("test.metric", 1, nil) // triggers initialization

	if err := ConfigureTelemetry("https://collector.example.com"); err == nil {
		t.Fatal("expected error when changing the endpoint after initialization")
	}
	if err := ConfigureTelemetry(server.URL); err != nil {
		t.Fatalf("re-configuring the same endpoint should succeed: %v", err)
	}
}

// dummyTelemetryServer returns a local endpoint that accepts all OTLP exports.
func dummyTelemetryServer(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

type telemetryRequest struct {
	path      string
	userAgent string
}

func TestTelemetryExportsMetricsAndTracesWithoutAuth(t *testing.T) {
	var mu sync.Mutex
	var requests []telemetryRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, telemetryRequest{
			path:      r.URL.Path,
			userAgent: r.Header.Get("User-Agent"),
		})
		mu.Unlock()
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("expected no Authorization header, got %q", auth)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	enableTestTelemetry(t, server.URL+"/otlp/")
	setResourceContext("app-1", "eu-central")

	span := startSpan("test.span", map[string]any{"app_id": "app-1"})
	if span == nil {
		t.Fatal("expected a span when telemetry is enabled")
	}
	addSpanEvent(span, "test.event", nil)
	finishSpan(span, map[string]any{"ok": true}, nil)
	recordMetric("test.metric", 42, map[string]any{"region": "eu-central"})

	ForceFlushTelemetry()

	mu.Lock()
	defer mu.Unlock()
	var sawTraces, sawMetrics bool
	for _, req := range requests {
		if req.userAgent != "spatius-go-sdk" {
			t.Errorf("expected User-Agent spatius-go-sdk, got %q", req.userAgent)
		}
		switch req.path {
		case "/otlp/v1/traces":
			sawTraces = true
		case "/otlp/v1/metrics":
			sawMetrics = true
		}
	}
	if !sawTraces {
		t.Error("expected an export request to /otlp/v1/traces")
	}
	if !sawMetrics {
		t.Error("expected an export request to /otlp/v1/metrics")
	}
}

func TestInjectTraceContextProducesW3CTraceparent(t *testing.T) {
	enableTestTelemetry(t, dummyTelemetryServer(t))

	span := startSpan("test.span", nil)
	if span == nil {
		t.Fatal("expected a span when telemetry is enabled")
	}
	defer finishSpan(span, nil, nil)

	carrier := injectTraceContext(span)
	traceparent := carrier["traceparent"]
	if !regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`).MatchString(traceparent) {
		t.Fatalf("expected W3C traceparent, got %q", traceparent)
	}
}

func TestTelemetryFailuresDoNotEscape(t *testing.T) {
	// Uninitialized and disabled: every helper must be a safe no-op.
	enableTestTelemetry(t, "")
	startSpan("noop", nil)
	finishSpan(nil, nil, nil)
	addSpanEvent(nil, "noop", nil)
	if carrier := injectTraceContext(nil); carrier != nil {
		t.Fatalf("expected nil carrier for nil span, got %v", carrier)
	}
	recordMetric("test.metric", 1, nil)
	ForceFlushTelemetry()
	ShutdownTelemetry()
	ShutdownTelemetry() // idempotent
}

func TestSessionRequestTelemetryLifecycle(t *testing.T) {
	enableTestTelemetry(t, dummyTelemetryServer(t))

	session := NewAvatarSession(
		WithConsoleEndpointURL("https://console.example.com"),
		WithIngressEndpointURL("wss://api.example.com"),
	)

	traceContext := session.startRequestTelemetry("req-1")
	if traceContext["traceparent"] == "" {
		t.Fatal("expected traceparent for the first audio message")
	}
	if again := session.startRequestTelemetry("req-1"); again != nil {
		t.Fatal("expected no trace context for a duplicate req_id")
	}

	session.recordAudioSentTelemetry("req-1", 5, false)
	session.recordAudioSentTelemetry("req-1", 6, true)
	session.recordAnimationTelemetry("req-1", false)
	session.recordAnimationTelemetry("req-1", true) // finishes the request

	session.telemetryMu.Lock()
	remaining := len(session.requestTelemetry)
	session.telemetryMu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected request telemetry to be finished, %d entries remain", remaining)
	}

	// Unknown req_ids must be safe no-ops.
	session.finishRequestTelemetry("unknown", "animation_end", nil)
	session.recordAnimationTelemetry("unknown", true)
}

func TestFinishAllRequestTelemetryOnClose(t *testing.T) {
	enableTestTelemetry(t, dummyTelemetryServer(t))

	session := NewAvatarSession()
	session.startRequestTelemetry("req-1")
	session.startRequestTelemetry("req-2")

	if err := session.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	session.telemetryMu.Lock()
	remaining := len(session.requestTelemetry)
	session.telemetryMu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected all request telemetry to be finished, %d entries remain", remaining)
	}
}

func TestConfigureTelemetryNormalizesEndpoint(t *testing.T) {
	enableTestTelemetry(t, "https://collector.example.com/otlp/")

	telemetryState.Lock()
	endpoint := telemetryState.endpoint
	telemetryState.Unlock()
	if endpoint != "https://collector.example.com/otlp" {
		t.Fatalf("expected trailing slash to be trimmed, got %q", endpoint)
	}
	if !strings.HasPrefix(telemetrySignalEndpoint("metrics"), endpoint+"/v1/") {
		t.Fatal("signal endpoints must derive from the normalized base endpoint")
	}
}
