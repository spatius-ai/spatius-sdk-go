package spatiussdkgo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func stubWarmTLSConnection(t *testing.T, warmed *[]string, ok bool) {
	t.Helper()
	original := warmTLSConnection
	warmTLSConnection = func(_ context.Context, rawURL string, _ time.Duration) bool {
		if warmed != nil {
			*warmed = append(*warmed, rawURL)
		}
		return ok
	}
	t.Cleanup(func() { warmTLSConnection = original })
}

func stubResolveRegion(t *testing.T, region string) {
	t.Helper()
	original := resolveRegionFunc
	resolveRegionFunc = func(context.Context, string, string) (string, bool) { return region, false }
	t.Cleanup(func() { resolveRegionFunc = original })
}

func TestPrewarmResolvesAndCachesAutoRegion(t *testing.T) {
	clearBootstrapRegionCache(t)
	var bootstrapCalls atomic.Int32
	useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		bootstrapCalls.Add(1)
		_, _ = w.Write([]byte(`{"region": {"current": "eu-central", "candidates": ["us-west"]}}`))
	})
	stubWarmTLSConnection(t, nil, false)

	result := Prewarm(context.Background(), "app-1", WithPrewarmTLSWarmup(false))

	if result.Region != "eu-central" {
		t.Fatalf("expected region eu-central, got %q", result.Region)
	}
	if result.ConsoleEndpointURL != "https://console.eu-central.spatius.ai/v1/console" {
		t.Fatalf("unexpected console endpoint URL: %q", result.ConsoleEndpointURL)
	}
	if result.IngressEndpointURL != "wss://api.eu-central.spatius.ai/v2/driveningress" {
		t.Fatalf("unexpected ingress endpoint URL: %q", result.IngressEndpointURL)
	}
	if result.SessionTokenPrefetched {
		t.Fatal("expected no session token prefetch")
	}

	// The resolved region is cached: a later warm-up must not re-fetch.
	if result := Prewarm(context.Background(), "app-1", WithPrewarmTLSWarmup(false)); result.Region != "eu-central" {
		t.Fatalf("expected cached region eu-central, got %q", result.Region)
	}
	if got := bootstrapCalls.Load(); got != 1 {
		t.Fatalf("expected exactly one bootstrap call, got %d", got)
	}
}

func TestPrewarmConcreteRegionSkipsBootstrap(t *testing.T) {
	clearBootstrapRegionCache(t)
	useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("bootstrap must not be called for a concrete region")
		w.WriteHeader(http.StatusInternalServerError)
	})
	stubWarmTLSConnection(t, nil, false)

	result := Prewarm(context.Background(), "app-1",
		WithPrewarmRegion("ap-southeast"), WithPrewarmTLSWarmup(false))

	if result.Region != "ap-southeast" {
		t.Fatalf("expected region ap-southeast, got %q", result.Region)
	}
}

func TestPrewarmTLSWarmReportsWarmedHosts(t *testing.T) {
	clearBootstrapRegionCache(t)
	stubResolveRegion(t, "eu-central")
	var warmed []string
	stubWarmTLSConnection(t, &warmed, true)

	result := Prewarm(context.Background(), "app-1")

	expectedHosts := []string{"console.eu-central.spatius.ai", "api.eu-central.spatius.ai"}
	if !reflect.DeepEqual(result.TLSWarmed, expectedHosts) {
		t.Fatalf("expected warmed hosts %v, got %v", expectedHosts, result.TLSWarmed)
	}
	if len(warmed) != 2 {
		t.Fatalf("expected 2 warm-up connections, got %v", warmed)
	}
}

func TestPrewarmTLSWarmFailureIsBestEffort(t *testing.T) {
	clearBootstrapRegionCache(t)
	stubResolveRegion(t, "eu-central")
	stubWarmTLSConnection(t, nil, false)

	result := Prewarm(context.Background(), "app-1")

	if len(result.TLSWarmed) != 0 {
		t.Fatalf("expected no warmed hosts, got %v", result.TLSWarmed)
	}
	if result.Region != "eu-central" {
		t.Fatalf("expected region eu-central, got %q", result.Region)
	}
}

func TestPrewarmSessionTokenPrefetchConsumedByInit(t *testing.T) {
	clearBootstrapRegionCache(t)
	clearSessionTokenCacheForTest(t)

	var tokenCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		if r.URL.Path != sessionTokenPath {
			t.Errorf("expected session token path %s, got %s", sessionTokenPath, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(sessionTokenResponse{SessionToken: "tok-prefetched"})
	}))
	defer tokenServer.Close()

	result := Prewarm(context.Background(), "app-1",
		WithPrewarmAPIKey("api"),
		WithPrewarmSessionTokenPrefetch(true),
		WithPrewarmEndpointURLs(tokenServer.URL, ""),
		WithPrewarmTLSWarmup(false),
	)

	if !result.SessionTokenPrefetched {
		t.Fatal("expected the session token to be prefetched")
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("expected exactly one token request, got %d", got)
	}

	// The next Init with matching credentials reuses the prefetched token
	// instead of calling the console API again.
	session := NewAvatarSession(
		WithAPIKey("api"),
		WithAppID("app-1"),
		WithConsoleEndpointURL(tokenServer.URL),
	)
	if err := session.Init(context.Background()); err != nil {
		t.Fatalf("init returned error: %v", err)
	}
	if session.sessionToken != "tok-prefetched" {
		t.Fatalf("expected prefetched session token, got %q", session.sessionToken)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("expected init to reuse the prefetched token, got %d token requests", got)
	}
}

func TestPrewarmSessionTokenPrefetchFailureIsBestEffort(t *testing.T) {
	clearBootstrapRegionCache(t)
	clearSessionTokenCacheForTest(t)

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer tokenServer.Close()

	result := Prewarm(context.Background(), "app-1",
		WithPrewarmAPIKey("api"),
		WithPrewarmSessionTokenPrefetch(true),
		WithPrewarmEndpointURLs(tokenServer.URL, ""),
		WithPrewarmTLSWarmup(false),
	)

	if result.SessionTokenPrefetched {
		t.Fatal("expected session token prefetch to report failure")
	}
}

func TestPrewarmSessionTokenPrefetchRequiresAPIKey(t *testing.T) {
	clearBootstrapRegionCache(t)
	clearSessionTokenCacheForTest(t)
	stubResolveRegion(t, "eu-central")
	stubWarmTLSConnection(t, nil, false)

	result := Prewarm(context.Background(), "app-1",
		WithPrewarmSessionTokenPrefetch(true),
		WithPrewarmTLSWarmup(false),
	)

	if result.SessionTokenPrefetched {
		t.Fatal("expected prefetch to be skipped without an API key")
	}
	if result.Region != "eu-central" {
		t.Fatalf("expected region eu-central, got %q", result.Region)
	}
}

func TestPrewarmExplicitEndpointsSkipRegionResolution(t *testing.T) {
	clearBootstrapRegionCache(t)
	useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("bootstrap must not be called for explicit endpoint URLs")
		w.WriteHeader(http.StatusInternalServerError)
	})
	stubWarmTLSConnection(t, nil, false)

	result := Prewarm(context.Background(), "app-1",
		WithPrewarmEndpointURLs("https://console.example.com/v1/console",
			"wss://api.example.com/v2/driveningress"),
		WithPrewarmTLSWarmup(false),
	)

	if result.Region != "" {
		t.Fatalf("expected no resolved region for explicit endpoints, got %q", result.Region)
	}
	if result.ConsoleEndpointURL != "https://console.example.com/v1/console" {
		t.Fatalf("unexpected console endpoint URL: %q", result.ConsoleEndpointURL)
	}
	if result.IngressEndpointURL != "wss://api.example.com/v2/driveningress" {
		t.Fatalf("unexpected ingress endpoint URL: %q", result.IngressEndpointURL)
	}
}
