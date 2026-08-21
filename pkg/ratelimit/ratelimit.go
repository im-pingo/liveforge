package ratelimit

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Limiter implements a per-IP token bucket rate limiter.
type Limiter struct {
	rate           float64 // tokens per second
	burst          int     // max tokens
	trustedProxies []*net.IPNet
	mu             sync.Mutex
	visitors       map[string]*bucket
	stopCh         chan struct{}
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// New creates a Limiter that allows rate requests/sec with the given burst size.
// Starts a background goroutine to clean up stale entries.
func New(rate float64, burst int, trustedProxyValues ...string) *Limiter {
	trustedProxies := make([]*net.IPNet, 0, len(trustedProxyValues))
	for _, value := range trustedProxyValues {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if ip := net.ParseIP(value); ip != nil {
			bits := 128
			if ip4 := ip.To4(); ip4 != nil {
				ip = ip4
				bits = 32
			}
			value = fmt.Sprintf("%s/%d", ip, bits)
		}
		if _, network, err := net.ParseCIDR(value); err == nil {
			trustedProxies = append(trustedProxies, network)
		}
	}
	l := &Limiter{
		rate:           rate,
		burst:          burst,
		trustedProxies: trustedProxies,
		visitors:       make(map[string]*bucket),
		stopCh:         make(chan struct{}),
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
		ip := extractIPWithTrustedProxies(r, l.trustedProxies)
		if !l.Allow(ip) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractIPWithTrustedProxies honors forwarding headers only when the direct
// peer belongs to an explicitly configured trusted proxy network. This keeps
// client-controlled forwarding headers from bypassing the per-client limiter.
func extractIPWithTrustedProxies(r *http.Request, trusted []*net.IPNet) string {
	remote := remoteIP(r.RemoteAddr)
	peer := net.ParseIP(remote)
	for _, network := range trusted {
		if peer != nil && network.Contains(peer) {
			if xff := firstForwardedIP(r.Header.Get("X-Forwarded-For")); xff != "" {
				return xff
			}
			if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(xri) != nil {
				return xri
			}
			break
		}
	}
	return remote
}

func firstForwardedIP(value string) string {
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if net.ParseIP(item) != nil {
			return item
		}
	}
	return ""
}

func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
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
