package spatiussdkgo

// Process warm-up: move connection setup off the session-start critical path.
//
// Prewarm performs the network work that would otherwise happen inside
// AvatarSession.Init/Start at dispatch time:
//
//   - resolves an "auto" region via the bootstrap API (cached process-wide for
//     regionCacheTTL, so later Init calls skip the HTTP round trip),
//   - optionally opens throwaway TLS connections to the console and ingress
//     hosts (priming the DNS resolver, the network path, and the shared TLS
//     session cache used by both HTTP and WebSocket connections),
//   - optionally prefetches a session token so the next Init skips the console
//     API entirely.
//
// Everything is best-effort: failures are logged and reported in the result,
// never returned, so warm-up can run safely in worker prewarm hooks.

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// PrewarmResult is the outcome of a Prewarm call. Fields report what actually
// succeeded.
type PrewarmResult struct {
	// Region is the concrete region sessions will use ("" when it could not
	// be resolved).
	Region string
	// ConsoleEndpointURL is the resolved console API URL.
	ConsoleEndpointURL string
	// IngressEndpointURL is the resolved ingress WebSocket URL.
	IngressEndpointURL string
	// TLSWarmed lists the endpoint hosts a warm-up TLS connection was
	// established to.
	TLSWarmed []string
	// SessionTokenPrefetched reports whether a session token was fetched and
	// cached for later Init calls.
	SessionTokenPrefetched bool
}

type prewarmConfig struct {
	apiKey               string
	region               string
	consoleEndpointURL   string
	ingressEndpointURL   string
	warmTLS              bool
	prefetchSessionToken bool
	sessionExpireAt      time.Time
	timeout              time.Duration
}

// PrewarmOption applies a configuration change to a Prewarm call.
type PrewarmOption func(*prewarmConfig)

// WithPrewarmAPIKey sets the console API key. Required only when session token
// prefetch is enabled via WithPrewarmSessionTokenPrefetch.
func WithPrewarmAPIKey(apiKey string) PrewarmOption {
	return func(cfg *prewarmConfig) {
		cfg.apiKey = apiKey
	}
}

// WithPrewarmRegion sets the requested region; DefaultRegionRequest ("auto",
// the default) resolves and caches the recommended region via the bootstrap
// API.
func WithPrewarmRegion(region string) PrewarmOption {
	return func(cfg *prewarmConfig) {
		cfg.region = region
	}
}

// WithPrewarmEndpointURLs sets explicit console/ingress endpoint URLs. They
// override region composition; setting only one composes the other from
// DefaultRegion, mirroring session initialization.
func WithPrewarmEndpointURLs(consoleEndpointURL, ingressEndpointURL string) PrewarmOption {
	return func(cfg *prewarmConfig) {
		cfg.consoleEndpointURL = consoleEndpointURL
		cfg.ingressEndpointURL = ingressEndpointURL
	}
}

// WithPrewarmTLSWarmup toggles opening a throwaway TLS connection to each
// endpoint host (enabled by default).
func WithPrewarmTLSWarmup(enabled bool) PrewarmOption {
	return func(cfg *prewarmConfig) {
		cfg.warmTLS = enabled
	}
}

// WithPrewarmSessionTokenPrefetch toggles fetching a session token and caching
// it so the next AvatarSession.Init with matching credentials skips the
// console API round trip. It requires WithPrewarmAPIKey.
//
// It assumes the backend allows a token to back more than one session; keep it
// disabled if tokens are single-use.
func WithPrewarmSessionTokenPrefetch(enabled bool) PrewarmOption {
	return func(cfg *prewarmConfig) {
		cfg.prefetchSessionToken = enabled
	}
}

// WithPrewarmSessionExpireAt sets the expiration for the prefetched session
// token. Zero (the default) uses DefaultSessionTokenTTL from now.
func WithPrewarmSessionExpireAt(expireAt time.Time) PrewarmOption {
	return func(cfg *prewarmConfig) {
		cfg.sessionExpireAt = expireAt
	}
}

// WithPrewarmTimeout sets the per-operation timeout (default: 5s, the
// bootstrap resolution timeout).
func WithPrewarmTimeout(timeout time.Duration) PrewarmOption {
	return func(cfg *prewarmConfig) {
		cfg.timeout = timeout
	}
}

// Prewarm warms region resolution and connection state ahead of session
// creation (see the file-level comment). It never returns an error; failures
// are logged and reported in the result.
func Prewarm(ctx context.Context, appID string, opts ...PrewarmOption) (result PrewarmResult) {
	cfg := prewarmConfig{
		region:  DefaultRegionRequest,
		warmTLS: true,
		timeout: bootstrapTimeout,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	startedAt := time.Now()
	span := startSpan("spatius.prewarm", map[string]any{
		"app_id":                 appID,
		"region":                 cfg.region,
		"warm_tls":               cfg.warmTLS,
		"prefetch_session_token": cfg.prefetchSessionToken,
	})

	// A panic anywhere in warm-up must not take down the caller; report it as
	// a failed warm-up instead.
	defer func() {
		var err error
		if recovered := recover(); recovered != nil {
			log.Printf("spatiussdkgo: prewarm failed: %v", recovered)
			err = fmt.Errorf("prewarm failed: %v", recovered)
			result = PrewarmResult{}
		}

		metricAttributes := map[string]any{
			"success":                  err == nil,
			"tls_warmed":               len(result.TLSWarmed),
			"session_token_prefetched": result.SessionTokenPrefetched,
		}
		spanAttributes := map[string]any{
			"tls_warmed":               len(result.TLSWarmed),
			"session_token_prefetched": result.SessionTokenPrefetched,
		}
		if result.Region != "" {
			metricAttributes["region"] = result.Region
			spanAttributes["resolved_region"] = result.Region
		}
		recordMetric("spatius.prewarm.duration",
			float64(time.Since(startedAt).Milliseconds()), metricAttributes)
		finishSpan(span, spanAttributes, err)
	}()

	result = prewarmImpl(ctx, appID, &cfg)
	return result
}

func prewarmImpl(ctx context.Context, appID string, cfg *prewarmConfig) PrewarmResult {
	var result PrewarmResult

	sessionCfg := &SessionConfig{
		AppID:              appID,
		APIKey:             cfg.apiKey,
		Region:             cfg.region,
		ConsoleEndpointURL: cfg.consoleEndpointURL,
		IngressEndpointURL: cfg.ingressEndpointURL,
	}
	sessionCfg.applyEndpointDefaults()

	resolveCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
	resolveSessionEndpoints(resolveCtx, sessionCfg)
	cancel()

	if sessionCfg.hasConcreteRegion() {
		result.Region = sessionCfg.Region
	}
	result.ConsoleEndpointURL = sessionCfg.ConsoleEndpointURL
	result.IngressEndpointURL = sessionCfg.IngressEndpointURL

	if result.Region != "" {
		setResourceContext(appID, result.Region)
	}

	var wg sync.WaitGroup

	// Warm concurrently but keep the reported host order deterministic.
	var warmed []string
	if cfg.warmTLS {
		endpoints := make([]string, 0, 2)
		for _, endpoint := range []string{sessionCfg.ConsoleEndpointURL, sessionCfg.IngressEndpointURL} {
			if endpoint != "" {
				endpoints = append(endpoints, endpoint)
			}
		}
		warmed = make([]string, len(endpoints))
		for i, endpoint := range endpoints {
			wg.Add(1)
			go func(i int, endpoint string) {
				defer wg.Done()
				if !warmTLSConnection(ctx, endpoint, cfg.timeout) {
					return
				}
				host, _, err := hostPortForURL(endpoint)
				if err != nil {
					return
				}
				warmed[i] = host
			}(i, endpoint)
		}
	}

	var prefetched atomic.Bool
	if cfg.prefetchSessionToken {
		if cfg.apiKey == "" {
			log.Printf("spatiussdkgo: prewarm: session token prefetch requires an API key")
		} else {
			expireAt := cfg.sessionExpireAt
			if expireAt.IsZero() {
				expireAt = time.Now().Add(DefaultSessionTokenTTL)
			}
			sessionCfg.ExpireAt = expireAt
			wg.Add(1)
			go func() {
				defer wg.Done()
				tokenCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
				defer cancel()
				token, err := fetchSessionToken(tokenCtx, sessionCfg)
				if err != nil {
					log.Printf("spatiussdkgo: prewarm: session token prefetch failed: %v", err)
					return
				}
				storeSessionToken(sessionCfg.APIKey, sessionCfg.AppID,
					sessionCfg.ConsoleEndpointURL, token, expireAt)
				prefetched.Store(true)
			}()
		}
	}

	wg.Wait()

	for _, host := range warmed {
		if host != "" {
			result.TLSWarmed = append(result.TLSWarmed, host)
		}
	}
	result.SessionTokenPrefetched = prefetched.Load()
	return result
}
