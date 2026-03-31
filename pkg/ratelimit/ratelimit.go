package ratelimit

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Limiter implements a per-IP token bucket rate limiter.
type Limiter struct {
	rate     float64 // tokens per second
	burst    int     // max tokens
	mu       sync.Mutex
	visitors map[string]*bucket
	stopCh   chan struct{}
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// New creates a Limiter that allows rate requests/sec with the given burst size.
// Starts a background goroutine to clean up stale entries.
func New(rate float64, burst int) *Limiter {
	l := &Limiter{
		rate:     rate,
		burst:    burst,
		visitors: make(map[string]*bucket),
		stopCh:   make(chan struct{}),
	}
	go l.cleanup()
	return l
}

// Allow checks whether a request from the given IP is allowed.
func (l *Limiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.visitors[ip]
	now := time.Now()

	if !ok {
		l.visitors[ip] = &bucket{tokens: float64(l.burst) - 1, lastSeen: now}
		return true
	}

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.tokens += elapsed * l.rate
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.lastSeen = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// Wrap returns an http.Handler middleware that rate limits by client IP.
func (l *Limiter) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := extractIP(r)
		if !l.Allow(ip) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Close stops the background cleanup goroutine.
func (l *Limiter) Close() {
	close(l.stopCh)
}

func (l *Limiter) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-l.stopCh:
			return
		case <-ticker.C:
			l.mu.Lock()
			for ip, b := range l.visitors {
				if time.Since(b.lastSeen) > 3*time.Minute {
					delete(l.visitors, ip)
				}
			}
			l.mu.Unlock()
		}
	}
}

func extractIP(r *http.Request) string {
	// Check X-Forwarded-For first (first entry).
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := 0; i < len(xff) {
			for j := 0; j < len(xff); j++ {
				if xff[j] == ',' {
					return xff[:j]
				}
			}
			return xff
		}
	}

	// Check X-Real-IP.
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fall back to RemoteAddr.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
