package spatiussdkgo

import (
	"log"
	"sync"
	"time"
)

// Process-level cache for prefetched session tokens.
//
// Prewarm can fetch a session token ahead of time so that the next
// AvatarSession.Init skips the console API round trip. Entries are keyed by
// credentials and endpoint, and are reused until shortly before they expire.
//
// Note: reusing a token assumes the backend allows a session token to back
// more than one session over its lifetime. Keep token prefetch disabled if
// your deployment enforces one connection per token.

// tokenExpiryMargin is how long before its expiry a cached token is considered
// unusable, so a session never starts with a token that is about to lapse.
const tokenExpiryMargin = time.Minute

type sessionTokenCacheKey struct {
	apiKey             string
	appID              string
	consoleEndpointURL string
}

type cachedSessionToken struct {
	token    string
	expireAt time.Time
}

var sessionTokenCache = struct {
	sync.Mutex
	tokens map[sessionTokenCacheKey]cachedSessionToken
}{tokens: map[sessionTokenCacheKey]cachedSessionToken{}}

// storeSessionToken caches a session token for later AvatarSession.Init calls.
func storeSessionToken(apiKey, appID, consoleEndpointURL, token string, expireAt time.Time) {
	key := sessionTokenCacheKey{apiKey: apiKey, appID: appID, consoleEndpointURL: consoleEndpointURL}
	expireAt = expireAt.UTC()
	sessionTokenCache.Lock()
	sessionTokenCache.tokens[key] = cachedSessionToken{token: token, expireAt: expireAt}
	sessionTokenCache.Unlock()
	log.Printf("spatiussdkgo: cached session token (app_id=%s, expire_at=%s)", appID, expireAt.Format(time.RFC3339))
}

// cachedSessionTokenFor returns a fresh-enough cached token, or "" when
// missing or near expiry.
func cachedSessionTokenFor(apiKey, appID, consoleEndpointURL string) string {
	key := sessionTokenCacheKey{apiKey: apiKey, appID: appID, consoleEndpointURL: consoleEndpointURL}
	sessionTokenCache.Lock()
	defer sessionTokenCache.Unlock()
	entry, ok := sessionTokenCache.tokens[key]
	if !ok {
		return ""
	}
	if !time.Now().Before(entry.expireAt.Add(-tokenExpiryMargin)) {
		delete(sessionTokenCache.tokens, key)
		return ""
	}
	return entry.token
}

// clearSessionTokenCache drops all cached tokens (mainly useful in tests).
func clearSessionTokenCache() {
	sessionTokenCache.Lock()
	defer sessionTokenCache.Unlock()
	sessionTokenCache.tokens = map[sessionTokenCacheKey]cachedSessionToken{}
}
