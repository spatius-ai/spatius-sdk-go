package spatiussdkgo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func useBootstrapServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	original := bootstrapEndpoint
	bootstrapEndpoint = server.URL
	t.Cleanup(func() {
		bootstrapEndpoint = original
		server.Close()
	})
	return server
}

func clearBootstrapRegionCache(t *testing.T) {
	t.Helper()
	cacheBootstrapRegion("")
	t.Cleanup(func() { cacheBootstrapRegion("") })
}

func TestFetchBootstrapPostsExpectedPayload(t *testing.T) {
	var captured bootstrapRequest
	var capturedContentType string

	useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST request, got %s", r.Method)
		}
		capturedContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("failed to decode request payload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"region": {"current": "eu-central", "candidates": ["us-west"]}}`))
	})

	body, err := fetchBootstrap(context.Background(), "app-1", "1.2.3", DefaultRegionRequest, bootstrapTimeout)
	if err != nil {
		t.Fatalf("fetchBootstrap returned error: %v", err)
	}
	if body.Region == nil || body.Region.Current != "eu-central" {
		t.Fatalf("expected region.current eu-central, got %+v", body.Region)
	}
	if captured.AppID != "app-1" || captured.SDKVersion != "1.2.3" ||
		captured.Region != DefaultRegionRequest || captured.Platform != bootstrapPlatform {
		t.Fatalf("unexpected bootstrap payload: %+v", captured)
	}
	if capturedContentType != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %q", capturedContentType)
	}
}

func TestFetchBootstrapNon200RaisesBootstrapError(t *testing.T) {
	useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	_, err := fetchBootstrap(context.Background(), "app-1", "", DefaultRegionRequest, bootstrapTimeout)
	var bootstrapErr *BootstrapError
	if !errors.As(err, &bootstrapErr) {
		t.Fatalf("expected BootstrapError, got %v", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected error to mention status 503, got %v", err)
	}
}

func TestFetchBootstrapTransportErrorRaisesBootstrapError(t *testing.T) {
	server := useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {})
	server.Close() // force connection refused

	_, err := fetchBootstrap(context.Background(), "app-1", "", DefaultRegionRequest, bootstrapTimeout)
	var bootstrapErr *BootstrapError
	if !errors.As(err, &bootstrapErr) {
		t.Fatalf("expected BootstrapError, got %v", err)
	}
}

func TestFetchBootstrapInvalidJSONRaisesBootstrapError(t *testing.T) {
	useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`["not", "a", "dict"]`))
	})

	_, err := fetchBootstrap(context.Background(), "app-1", "", DefaultRegionRequest, bootstrapTimeout)
	var bootstrapErr *BootstrapError
	if !errors.As(err, &bootstrapErr) {
		t.Fatalf("expected BootstrapError, got %v", err)
	}
}

func TestResolveRegionConcreteRegionSkipsBootstrap(t *testing.T) {
	clearBootstrapRegionCache(t)
	server := useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("bootstrap must not be called for a concrete region")
		w.WriteHeader(http.StatusInternalServerError)
	})
	server.Close() // any call would fail loudly

	region := resolveRegion(context.Background(), "app-1", "eu-central")
	if region != "eu-central" {
		t.Fatalf("expected eu-central, got %q", region)
	}
}

func TestResolveRegionAutoResolvesAndCaches(t *testing.T) {
	clearBootstrapRegionCache(t)
	useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"region": {"current": "ap-southeast"}}`))
	})

	region := resolveRegion(context.Background(), "app-1", DefaultRegionRequest)
	if region != "ap-southeast" {
		t.Fatalf("expected ap-southeast, got %q", region)
	}
	if cached, _ := cachedBootstrapRegion(); cached != "ap-southeast" {
		t.Fatalf("expected cached region ap-southeast, got %q", cached)
	}
}

func TestResolveRegionEmptyTreatedAsAuto(t *testing.T) {
	clearBootstrapRegionCache(t)
	called := false
	useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"region": {"current": "eu-central"}}`))
	})

	region := resolveRegion(context.Background(), "app-1", "")
	if !called {
		t.Fatal("expected bootstrap to be called for an empty region")
	}
	if region != "eu-central" {
		t.Fatalf("expected eu-central, got %q", region)
	}
}

func TestResolveRegionFailureFallsBackToDefaultRegion(t *testing.T) {
	clearBootstrapRegionCache(t)
	useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	region := resolveRegion(context.Background(), "app-1", DefaultRegionRequest)
	if region != DefaultRegion {
		t.Fatalf("expected fallback to %q, got %q", DefaultRegion, region)
	}
}

func TestResolveRegionFailureFallsBackToCachedRegion(t *testing.T) {
	clearBootstrapRegionCache(t)
	// Seed a stale cache entry: it must not serve as a fresh hit, but must
	// still be the failure fallback.
	bootstrapCache.Lock()
	bootstrapCache.region = "eu-central"
	bootstrapCache.cachedAt = time.Time{}
	bootstrapCache.Unlock()
	useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	region, cacheHit := resolveRegionWithCacheInfo(context.Background(), "app-1", DefaultRegionRequest)
	if region != "eu-central" {
		t.Fatalf("expected fallback to cached eu-central, got %q", region)
	}
	if cacheHit {
		t.Fatal("expected cacheHit=false for a stale-cache failure fallback")
	}
}

func TestResolveRegionFreshCacheHitSkipsBootstrap(t *testing.T) {
	clearBootstrapRegionCache(t)
	server := useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"region": {"current": "eu-central"}}`))
	})

	region, cacheHit := resolveRegionWithCacheInfo(context.Background(), "app-1", DefaultRegionRequest)
	if region != "eu-central" || cacheHit {
		t.Fatalf("expected (eu-central, false) from the first resolution, got (%q, %t)", region, cacheHit)
	}

	// Within the TTL the bootstrap API must not be contacted again.
	server.Close()
	region, cacheHit = resolveRegionWithCacheInfo(context.Background(), "app-1", DefaultRegionRequest)
	if region != "eu-central" {
		t.Fatalf("expected cached eu-central, got %q", region)
	}
	if !cacheHit {
		t.Fatal("expected cacheHit=true for a fresh cache hit")
	}
}

func TestResolveRegionStaleCacheRefetches(t *testing.T) {
	clearBootstrapRegionCache(t)
	bootstrapCache.Lock()
	bootstrapCache.region = "eu-central"
	bootstrapCache.cachedAt = time.Now().Add(-regionCacheTTL - time.Minute)
	bootstrapCache.Unlock()
	useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"region": {"current": "ap-southeast"}}`))
	})

	region, cacheHit := resolveRegionWithCacheInfo(context.Background(), "app-1", DefaultRegionRequest)
	if region != "ap-southeast" {
		t.Fatalf("expected refetched ap-southeast, got %q", region)
	}
	if cacheHit {
		t.Fatal("expected cacheHit=false after a stale cache refetch")
	}
}

func TestResolveRegionMissingRegionCurrentFallsBack(t *testing.T) {
	clearBootstrapRegionCache(t)
	useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"time_sync": {"server_receive_ms": 1, "server_send_ms": 2}}`))
	})

	region := resolveRegion(context.Background(), "app-1", DefaultRegionRequest)
	if region != DefaultRegion {
		t.Fatalf("expected fallback to %q, got %q", DefaultRegion, region)
	}
}

func TestEnsureRegionResolvedResolvesAutoRegion(t *testing.T) {
	original := resolveRegionFunc
	resolveRegionFunc = func(context.Context, string, string) (string, bool) { return "eu-central", false }
	t.Cleanup(func() { resolveRegionFunc = original })

	session := NewAvatarSession(WithAppID("app-1"))
	cfg := session.Config()
	if cfg.Region != DefaultRegionRequest {
		t.Fatalf("expected region %q before resolution, got %q", DefaultRegionRequest, cfg.Region)
	}
	if cfg.ConsoleEndpointURL != "" || cfg.IngressEndpointURL != "" {
		t.Fatal("expected endpoint URLs to stay uncomposed before region resolution")
	}

	session.ensureRegionResolved(context.Background())

	cfg = session.Config()
	if cfg.Region != "eu-central" {
		t.Fatalf("expected region eu-central, got %q", cfg.Region)
	}
	if cfg.ConsoleEndpointURL != "https://console.eu-central.spatius.ai/v1/console" {
		t.Fatalf("unexpected console endpoint URL: %q", cfg.ConsoleEndpointURL)
	}
	if cfg.IngressEndpointURL != "wss://api.eu-central.spatius.ai/v2/driveningress" {
		t.Fatalf("unexpected ingress endpoint URL: %q", cfg.IngressEndpointURL)
	}
}

func TestEnsureRegionResolvedSkipsBootstrapForConcreteRegion(t *testing.T) {
	original := resolveRegionFunc
	resolveRegionFunc = func(context.Context, string, string) (string, bool) {
		t.Error("bootstrap must not be called for a concrete region")
		return "", false
	}
	t.Cleanup(func() { resolveRegionFunc = original })

	session := NewAvatarSession(WithRegion("eu-central"))
	session.ensureRegionResolved(context.Background())

	cfg := session.Config()
	if cfg.Region != "eu-central" {
		t.Fatalf("expected region eu-central, got %q", cfg.Region)
	}
	if cfg.ConsoleEndpointURL != "https://console.eu-central.spatius.ai/v1/console" {
		t.Fatalf("unexpected console endpoint URL: %q", cfg.ConsoleEndpointURL)
	}
}

func TestEnsureRegionResolvedSkipsBootstrapForExplicitURLs(t *testing.T) {
	original := resolveRegionFunc
	resolveRegionFunc = func(context.Context, string, string) (string, bool) {
		t.Error("bootstrap must not be called for explicit endpoint URLs")
		return "", false
	}
	t.Cleanup(func() { resolveRegionFunc = original })

	session := NewAvatarSession(
		WithConsoleEndpointURL("https://console.example.com/v1/console"),
		WithIngressEndpointURL("wss://api.example.com/v2/driveningress"),
	)
	session.ensureRegionResolved(context.Background())

	cfg := session.Config()
	if cfg.ConsoleEndpointURL != "https://console.example.com/v1/console" {
		t.Fatalf("unexpected console endpoint URL: %q", cfg.ConsoleEndpointURL)
	}
	if cfg.IngressEndpointURL != "wss://api.example.com/v2/driveningress" {
		t.Fatalf("unexpected ingress endpoint URL: %q", cfg.IngressEndpointURL)
	}
}

func TestEnsureRegionResolvedPartialExplicitURLsComposeFromDefaultRegion(t *testing.T) {
	original := resolveRegionFunc
	resolveRegionFunc = func(context.Context, string, string) (string, bool) {
		t.Error("bootstrap must not be called for partially explicit endpoint URLs")
		return "", false
	}
	t.Cleanup(func() { resolveRegionFunc = original })

	session := NewAvatarSession(WithConsoleEndpointURL("https://console.example.com/v1/console"))
	session.ensureRegionResolved(context.Background())

	cfg := session.Config()
	// Historical behavior: the missing ingress URL falls back to us-west.
	if cfg.IngressEndpointURL != "wss://api.us-west.spatius.ai/v2/driveningress" {
		t.Fatalf("unexpected ingress endpoint URL: %q", cfg.IngressEndpointURL)
	}
}

func TestInitResolvesAutoRegionViaBootstrap(t *testing.T) {
	clearBootstrapRegionCache(t)
	useBootstrapServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"region": {"current": "eu-central"}}`))
	})

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(sessionTokenResponse{SessionToken: "tok"})
	}))
	defer tokenServer.Close()

	session := NewAvatarSession(
		WithAPIKey("api"),
		WithAppID("app-1"),
		WithExpireAt(time.Now().Add(5*time.Minute)),
	)
	// Region resolution composes both endpoint URLs; redirect the console API
	// to the local token server after resolution but before the token request.
	session.ensureRegionResolved(context.Background())
	session.config.ConsoleEndpointURL = tokenServer.URL

	if _, err := session.init(context.Background()); err != nil {
		t.Fatalf("init returned error: %v", err)
	}
	if session.sessionToken != "tok" {
		t.Fatalf("expected session token tok, got %q", session.sessionToken)
	}
	if session.config.Region != "eu-central" {
		t.Fatalf("expected region eu-central, got %q", session.config.Region)
	}
	if session.config.IngressEndpointURL != "wss://api.eu-central.spatius.ai/v2/driveningress" {
		t.Fatalf("unexpected ingress endpoint URL: %q", session.config.IngressEndpointURL)
	}
}
