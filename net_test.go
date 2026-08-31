package spatiussdkgo

import (
	"context"
	"testing"
	"time"
)

func TestHostPortForURL(t *testing.T) {
	tests := []struct {
		rawURL   string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{"https://console.us-west.spatius.ai/v1/console", "console.us-west.spatius.ai", 443, false},
		{"wss://api.us-west.spatius.ai/v2/driveningress", "api.us-west.spatius.ai", 443, false},
		{"https://console.test:8443/v1/console", "console.test", 8443, false},
		{"api.example.com", "api.example.com", 443, false},
		{"http://127.0.0.1:8080", "127.0.0.1", 8080, false},
		{"://no-host", "", 0, true},
		{"", "", 0, true},
	}

	for _, tt := range tests {
		host, port, err := hostPortForURL(tt.rawURL)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("hostPortForURL(%q): expected error, got %s:%d", tt.rawURL, host, port)
			}
			continue
		}
		if err != nil {
			t.Fatalf("hostPortForURL(%q): unexpected error: %v", tt.rawURL, err)
		}
		if host != tt.wantHost || port != tt.wantPort {
			t.Fatalf("hostPortForURL(%q): expected %s:%d, got %s:%d",
				tt.rawURL, tt.wantHost, tt.wantPort, host, port)
		}
	}
}

func TestWarmTLSConnectionUnparseableURL(t *testing.T) {
	if warmTLSConnection(context.Background(), "://bad", time.Second) {
		t.Fatal("expected warm-up to report failure for an unparseable URL")
	}
}

func TestWarmTLSConnectionRefused(t *testing.T) {
	// Port 1 on loopback refuses connections immediately.
	if warmTLSConnection(context.Background(), "https://127.0.0.1:1", time.Second) {
		t.Fatal("expected warm-up to report failure for a refused connection")
	}
}
