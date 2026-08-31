package spatiussdkgo

import (
	"testing"
	"time"
)

func clearSessionTokenCacheForTest(t *testing.T) {
	t.Helper()
	clearSessionTokenCache()
	t.Cleanup(clearSessionTokenCache)
}

func TestSessionTokenCacheHit(t *testing.T) {
	clearSessionTokenCacheForTest(t)

	expireAt := time.Now().Add(time.Hour)
	storeSessionToken("api", "app-1", "https://console.test", "tok-1", expireAt)

	if token := cachedSessionTokenFor("api", "app-1", "https://console.test"); token != "tok-1" {
		t.Fatalf("expected cached token tok-1, got %q", token)
	}
}

func TestSessionTokenCacheMissForUnknownKey(t *testing.T) {
	clearSessionTokenCacheForTest(t)

	storeSessionToken("api", "app-1", "https://console.test", "tok-1", time.Now().Add(time.Hour))

	if token := cachedSessionTokenFor("other-api", "app-1", "https://console.test"); token != "" {
		t.Fatalf("expected cache miss for a different API key, got %q", token)
	}
	if token := cachedSessionTokenFor("api", "app-2", "https://console.test"); token != "" {
		t.Fatalf("expected cache miss for a different app ID, got %q", token)
	}
	if token := cachedSessionTokenFor("api", "app-1", "https://other.test"); token != "" {
		t.Fatalf("expected cache miss for a different endpoint, got %q", token)
	}
}

func TestSessionTokenCacheEvictsNearExpiry(t *testing.T) {
	clearSessionTokenCacheForTest(t)

	// Inside the expiry margin: unusable and evicted.
	storeSessionToken("api", "app-1", "https://console.test", "tok-1",
		time.Now().Add(tokenExpiryMargin/2))
	if token := cachedSessionTokenFor("api", "app-1", "https://console.test"); token != "" {
		t.Fatalf("expected near-expiry token to be unusable, got %q", token)
	}
	if len(sessionTokenCache.tokens) != 0 {
		t.Fatal("expected the near-expiry entry to be evicted")
	}

	// Beyond the margin: still usable.
	storeSessionToken("api", "app-1", "https://console.test", "tok-2",
		time.Now().Add(2*tokenExpiryMargin))
	if token := cachedSessionTokenFor("api", "app-1", "https://console.test"); token != "tok-2" {
		t.Fatalf("expected cached token tok-2, got %q", token)
	}
}

func TestClearSessionTokenCache(t *testing.T) {
	clearSessionTokenCacheForTest(t)

	storeSessionToken("api", "app-1", "https://console.test", "tok-1", time.Now().Add(time.Hour))
	clearSessionTokenCache()
	if token := cachedSessionTokenFor("api", "app-1", "https://console.test"); token != "" {
		t.Fatalf("expected empty cache after clear, got %q", token)
	}
}
