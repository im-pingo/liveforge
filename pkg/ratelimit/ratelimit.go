package ratelimit

import (
	"net"
	"net/http"
	"strings"
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
	stopOnce sync.Once
	trusted  []*net.IPNet
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// New creates a Limiter that allows rate requests/sec with the given burst size.
// Starts a background goroutine to clean up stale entries.
func New(rate float64, burst int) *Limiter {
	return NewWithTrustedProxies(rate, burst, nil)
}

// NewWithTrustedProxies creates a limiter that accepts forwarded client
// headers only when the direct peer belongs to a configured proxy network.
func NewWithTrustedProxies(rate float64, burst int, trustedProxies []string) *Limiter {
	if burst <= 0 {
		burst = 1
	}
	l := &Limiter{
		rate:     rate,
		burst:    burst,
		visitors: make(map[string]*bucket),
		stopCh:   make(chan struct{}),
		trusted:  parseTrustedProxies(trustedProxies),
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
		ip := extractIPWithTrustedProxies(r, l.trusted)
		if !l.Allow(ip) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AllowRequest applies the limiter to the request's resolved client address.
func (l *Limiter) AllowRequest(r *http.Request) bool {
	return l.Allow(extractIPWithTrustedProxies(r, l.trusted))
}

// Close stops the background cleanup goroutine.
func (l *Limiter) Close() {
	l.stopOnce.Do(func() { close(l.stopCh) })
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

func extractIPWithTrustedProxies(r *http.Request, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer != nil && isTrustedProxy(peer, trusted) {
		if value := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); value != "" {
			if xff, ok := forwardedClientIP(value, trusted); ok {
				return xff
			}
			return host
		}
		if xri := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); xri != nil {
			return xri.String()
		}
	}
	return host
}

func forwardedClientIP(value string, trusted []*net.IPNet) (string, bool) {
	hops := strings.Split(value, ",")
	leftmost := ""
	for index := len(hops) - 1; index >= 0; index-- {
		ip := net.ParseIP(strings.TrimSpace(hops[index]))
		if ip == nil {
			return "", false
		}
		leftmost = ip.String()
		if !isTrustedProxy(ip, trusted) {
			return leftmost, true
		}
	}
	return leftmost, leftmost != ""
}

func parseTrustedProxies(values []string) []*net.IPNet {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if strings.Contains(value, "/") {
			if _, network, err := net.ParseCIDR(value); err == nil {
				result = append(result, network)
			}
			continue
		}
		if ip := net.ParseIP(value); ip != nil {
			bits := 128
			if ip.To4() != nil {
				bits = 32
			}
			result = append(result, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return result
}

func isTrustedProxy(ip net.IP, trusted []*net.IPNet) bool {
	for _, network := range trusted {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
