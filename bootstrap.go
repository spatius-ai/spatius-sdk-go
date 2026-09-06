package spatiussdkgo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Global bootstrap entry: region resolution via the global scheduling API.
//
// A single POST to the global bootstrap endpoint returns both the recommended
// ingress region (automatic regional scheduling) and server time-sync fields.
// This SDK currently consumes only the region field.
//
// Region resolution semantics (aligned with the other SDKs):
//
//   - A concrete requested region is used as-is; bootstrap is not called.
//   - "auto" calls bootstrap once and uses region.current from the response.
//     Successful resolutions are cached for regionCacheTTL so repeated
//     sessions (or a warm-up call ahead of dispatch) skip the HTTP round trip.
//   - On failure (network error, timeout, non-200, or a malformed response) it
//     falls back to the last successfully resolved region cached in this
//     process (even if stale), or to DefaultRegion when nothing is cached. It
//     never fails, so session initialization is never blocked by region
//     scheduling.

const (
	// bootstrapTimeout is the timeout for a single bootstrap request.
	bootstrapTimeout = 5 * time.Second

	// regionCacheTTL is how long a successfully resolved "auto" region is
	// reused before bootstrap is called again. Bounded so regional failovers
	// are picked up by long-lived processes.
	regionCacheTTL = 5 * time.Minute

	// bootstrapPlatform is the platform identifier reported to the backend.
	bootstrapPlatform = "go"
)

// bootstrapEndpoint is the global bootstrap entry (region scheduling + server
// time sync). It is a package variable so tests can point it at a local server.
var bootstrapEndpoint = "https://global.spatialwalk.top/bootstrap"

// BootstrapError is returned when the bootstrap request fails or returns an
// unusable response.
type BootstrapError struct {
	Message string
}

func (e *BootstrapError) Error() string {
	return e.Message
}

type bootstrapRequest struct {
	AppID      string `json:"app_id"`
	SDKVersion string `json:"sdk_version"`
	Region     string `json:"region"`
	Platform   string `json:"platform"`
}

type bootstrapResponse struct {
	Region *struct {
		Current    string   `json:"current"`
		Candidates []string `json:"candidates"`
	} `json:"region"`
	TimeSync map[string]any `json:"time_sync"`
}

var bootstrapCache = struct {
	sync.Mutex
	// region is the last region successfully resolved for "auto", reused as
	// the fallback when a later resolution fails. Process-level equivalent of
	// the web SDK's localStorage cache. A zero cachedAt marks the cached value
	// as stale, only usable as a failure fallback.
	region   string
	cachedAt time.Time
}{}

// cachedBootstrapRegion returns the cached region and whether it is fresh
// enough to skip the bootstrap round trip.
func cachedBootstrapRegion() (region string, fresh bool) {
	bootstrapCache.Lock()
	defer bootstrapCache.Unlock()
	return bootstrapCache.region,
		!bootstrapCache.cachedAt.IsZero() && time.Since(bootstrapCache.cachedAt) < regionCacheTTL
}

// cacheBootstrapRegion records a successfully resolved region. An empty region
// clears the cache.
func cacheBootstrapRegion(region string) {
	bootstrapCache.Lock()
	defer bootstrapCache.Unlock()
	bootstrapCache.region = region
	if region == "" {
		bootstrapCache.cachedAt = time.Time{}
	} else {
		bootstrapCache.cachedAt = time.Now()
	}
}

// fetchBootstrap sends one bootstrap request and returns the parsed response
// body. It returns a BootstrapError on transport errors, timeouts, non-200
// responses, or malformed bodies.
func fetchBootstrap(ctx context.Context, appID, version, region string, timeout time.Duration) (*bootstrapResponse, error) {
	if version == "" {
		version = sdkVersion()
	}
	if timeout <= 0 {
		timeout = bootstrapTimeout
	}

	payload := bootstrapRequest{
		AppID:      appID,
		SDKVersion: version,
		Region:     region,
		Platform:   bootstrapPlatform,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &BootstrapError{Message: fmt.Sprintf("encode bootstrap request: %v", err)}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	startedAt := time.Now()
	statusCode := 0
	defer func() {
		// Report the request duration for both success and failure paths.
		// region scheduling failures must never affect session initialization,
		// and this metric is the primary signal when they happen.
		recordHTTPClientDuration(
			"/bootstrap",
			http.MethodPost,
			float64(time.Since(startedAt).Milliseconds()),
			statusCode,
			"global.spatialwalk.top",
		)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bootstrapEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &BootstrapError{Message: fmt.Sprintf("create bootstrap request: %v", err)}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := sharedHTTPClient.Do(req)
	if err != nil {
		return nil, &BootstrapError{Message: fmt.Sprintf("bootstrap request failed: %v", err)}
	}
	defer resp.Body.Close() // nolint:errcheck

	statusCode = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		setResourceContext(appID, bootstrapTelemetryRegion(nil, region))
		return nil, &BootstrapError{Message: fmt.Sprintf("bootstrap HTTP %d", resp.StatusCode)}
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		setResourceContext(appID, bootstrapTelemetryRegion(nil, region))
		return nil, &BootstrapError{Message: fmt.Sprintf("read bootstrap response: %v", err)}
	}

	var parsed bootstrapResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		setResourceContext(appID, bootstrapTelemetryRegion(nil, region))
		return nil, &BootstrapError{Message: "bootstrap response is not a JSON object"}
	}
	setResourceContext(appID, bootstrapTelemetryRegion(&parsed, region))
	return &parsed, nil
}

// bootstrapTelemetryRegion returns the region reported in telemetry resource
// metadata: the resolved region on success, otherwise the requested region or
// DefaultRegion for "auto".
func bootstrapTelemetryRegion(body *bootstrapResponse, requestedRegion string) string {
	if body != nil && body.Region != nil && body.Region.Current != "" {
		return body.Region.Current
	}
	if requestedRegion != "" && requestedRegion != DefaultRegionRequest {
		return requestedRegion
	}
	return DefaultRegion
}

// resolveRegionWithCacheInfo resolves the requested region into a concrete
// ingress region, and reports whether the result was served from a fresh
// warm-cache entry (i.e. without contacting the bootstrap API).
//
// The returned region is never "auto". cacheHit is true only for a fresh
// positive cache hit; bootstrap fetches, failure fallbacks (including
// stale-cache fallbacks), and concrete pinned regions all report false.
// Resolution failures fall back to the cached region (even when stale) or
// DefaultRegion and never fail.
func resolveRegionWithCacheInfo(ctx context.Context, appID, requestedRegion string) (string, bool) {
	requested := strings.TrimSpace(requestedRegion)
	if requested != "" && requested != DefaultRegionRequest {
		// User pinned a concrete region - use it directly, no scheduling.
		return requested, false
	}

	if cached, fresh := cachedBootstrapRegion(); cached != "" && fresh {
		log.Printf("spatiussdkgo: [RegionResolver] auto -> %s (cached)", cached)
		return cached, true
	}

	body, err := fetchBootstrap(ctx, appID, "", DefaultRegionRequest, bootstrapTimeout)
	if err == nil && body.Region != nil && body.Region.Current != "" {
		cacheBootstrapRegion(body.Region.Current)
		log.Printf("spatiussdkgo: [RegionResolver] auto -> %s", body.Region.Current)
		return body.Region.Current, false
	}
	if err == nil {
		err = &BootstrapError{Message: "bootstrap response missing region.current"}
	}

	cached, _ := cachedBootstrapRegion()
	fallback := cached
	if fallback == "" {
		fallback = DefaultRegion
	}
	log.Printf(
		"spatiussdkgo: [RegionResolver] auto resolve failed, falling back to %s (from_cache=%t): %v",
		fallback, cached != "", err,
	)
	return fallback, false
}

// resolveRegion resolves the requested region into a concrete ingress region.
// It is resolveRegionWithCacheInfo without the cache-hit indicator.
func resolveRegion(ctx context.Context, appID, requestedRegion string) string {
	region, _ := resolveRegionWithCacheInfo(ctx, appID, requestedRegion)
	return region
}

// resolveRegionFunc is the resolver used by session initialization. It is a
// package variable so tests can stub region resolution.
var resolveRegionFunc = resolveRegionWithCacheInfo

// ensureRegionResolved resolves an "auto" region via the global bootstrap API
// and composes the endpoint URLs from the result.
//
// Resolution only runs when the user relies on region-composed endpoints
// without pinning a concrete region. It is skipped entirely when:
//
//   - both endpoint URLs were configured explicitly, or
//   - a concrete region was configured (URLs were composed at construction).
//
// When only some endpoint URLs are explicit, the missing ones are composed
// from DefaultRegion (preserving the historical default) instead of calling
// bootstrap. Resolution failures never fail: the region falls back to the last
// cached region or DefaultRegion.
//
// Returns nil when no region resolution was needed, true when an "auto"
// region was served from the fresh process cache, and false when the
// bootstrap API was contacted (successfully or not).
func resolveSessionEndpoints(ctx context.Context, config *SessionConfig) *bool {
	if config.ConsoleEndpointURL != "" && config.IngressEndpointURL != "" {
		// Fully explicit endpoints - nothing to compose.
		return nil
	}

	if config.hasConcreteRegion() {
		// Concrete region - compose any URLs still missing.
		config.applyResolvedRegion(config.Region)
		return nil
	}

	if config.ConsoleEndpointURL != "" || config.IngressEndpointURL != "" {
		// Partially explicit endpoints with an auto region: compose the
		// remaining URLs from the default region instead of scheduling.
		config.applyResolvedRegion(DefaultRegion)
		return nil
	}

	region, cacheHit := resolveRegionFunc(ctx, config.AppID, config.Region)
	config.applyResolvedRegion(region)
	return &cacheHit
}

// ensureRegionResolved resolves the session's "auto" region and composes its
// endpoint URLs (see resolveSessionEndpoints). It returns the cache-hit
// indicator.
func (s *AvatarSession) ensureRegionResolved(ctx context.Context) *bool {
	return resolveSessionEndpoints(ctx, s.config)
}
