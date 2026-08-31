package spatiussdkgo

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Shared networking primitives for the SDK.
//
// A single process-wide TLS client config is shared by every HTTP request and
// WebSocket connection so the TLS session cache (session tickets) is reused
// between them, and so warm-up connections made before dispatch benefit the
// real ones. The HTTP transport additionally pools keep-alive connections;
// DNS caching is left to the OS resolver.

const (
	// sessionTokenTimeout bounds the session-token exchange with the console
	// API, so Init cannot hang on an unresponsive console endpoint when the
	// caller's context has no deadline.
	sessionTokenTimeout = 10 * time.Second

	// tlsWarmTimeout bounds a single warm-up TLS connection.
	tlsWarmTimeout = 5 * time.Second
)

// sharedTLSConfig is the process-wide client TLS config. Sharing one config
// lets Go reuse TLS session tickets across connections, including warm-up
// connections made ahead of time.
var sharedTLSConfig = &tls.Config{
	MinVersion:         tls.VersionTLS12,
	ClientSessionCache: tls.NewLRUClientSessionCache(64),
}

var sharedHTTPTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	TLSClientConfig:       sharedTLSConfig,
}

// sharedHTTPClient serves the bootstrap and session-token APIs so they share
// keep-alive connections and the process-wide TLS session cache.
var sharedHTTPClient = &http.Client{Transport: sharedHTTPTransport}

// websocketDialer shares the process-wide TLS session cache with the HTTP
// client so wss session tickets from earlier connections (e.g. Prewarm) are
// reused. It otherwise mirrors websocket.DefaultDialer.
var websocketDialer = &websocket.Dialer{
	Proxy:            http.ProxyFromEnvironment,
	HandshakeTimeout: 45 * time.Second,
	TLSClientConfig:  sharedTLSConfig,
}

// hostPortForURL extracts (host, port) from an http(s):// or ws(s):// endpoint
// URL, defaulting to port 443.
func hostPortForURL(rawURL string) (string, int, error) {
	u := rawURL
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Hostname() == "" {
		return "", 0, fmt.Errorf("cannot determine host from URL: %q", rawURL)
	}
	port := 443
	if p := parsed.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil {
			return "", 0, fmt.Errorf("cannot determine port from URL: %q", rawURL)
		}
	}
	return parsed.Hostname(), port, nil
}

// warmTLSConnection opens and immediately closes a bare TLS connection to the
// URL's host. Best-effort warm-up: it primes the OS resolver cache, the
// network path to the edge, and the shared TLS session cache so the real
// connection during Start is cheaper. It never fails hard; it returns whether
// the connection was established.
//
// It is a package variable so tests can stub it.
var warmTLSConnection = func(ctx context.Context, rawURL string, timeout time.Duration) bool {
	host, port, err := hostPortForURL(rawURL)
	if err != nil {
		log.Printf("spatiussdkgo: TLS warm-up skipped for unparseable URL %q", rawURL)
		return false
	}
	if timeout <= 0 {
		timeout = tlsWarmTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: timeout},
		Config:    sharedTLSConfig,
	}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		log.Printf("spatiussdkgo: TLS warm-up to %s:%d failed: %v", host, port, err)
		return false
	}
	_ = conn.Close()
	log.Printf("spatiussdkgo: warmed TLS connection to %s:%d", host, port)
	return true
}
