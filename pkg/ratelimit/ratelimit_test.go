package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLimiterAllow(t *testing.T) {
	l := New(10, 5) // 10 req/s, burst of 5
	defer l.Close()

	ip := "192.168.1.1"

	// First 5 should be allowed (burst).
	for i := 0; i < 5; i++ {
		if !l.Allow(ip) {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}

	// 6th should be rejected (burst exhausted, no time to refill).
	if l.Allow(ip) {
		t.Fatal("6th request should be rejected")
	}
}

func TestLimiterDifferentIPs(t *testing.T) {
	l := New(1, 2) // 1 req/s, burst of 2
	defer l.Close()

	if !l.Allow("10.0.0.1") {
		t.Fatal("first IP first request should be allowed")
	}
	if !l.Allow("10.0.0.2") {
		t.Fatal("second IP first request should be allowed")
	}
}

func TestLimiterWrapMiddleware(t *testing.T) {
	l := New(100, 2) // burst of 2
	defer l.Close()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := l.Wrap(inner)

	// First 2 requests pass.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, w.Code)
		}
	}

	// 3rd request should be rate limited.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
}

func TestExtractIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		xri        string
		want       string
	}{
		{"remote addr", "192.168.1.1:12345", "", "", "192.168.1.1"},
		{"x-forwarded-for single", "10.0.0.1:80", "203.0.113.50", "", "203.0.113.50"},
		{"x-forwarded-for chain", "10.0.0.1:80", "203.0.113.50, 70.41.3.18", "", "203.0.113.50"},
		{"x-real-ip", "10.0.0.1:80", "", "198.51.100.178", "198.51.100.178"},
		{"xff takes priority over xri", "10.0.0.1:80", "203.0.113.50", "198.51.100.178", "203.0.113.50"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.xri != "" {
				r.Header.Set("X-Real-IP", tt.xri)
			}
			got := extractIP(r)
			if got != tt.want {
				t.Errorf("extractIP() = %q, want %q", got, tt.want)
			}
		})
	}
}
