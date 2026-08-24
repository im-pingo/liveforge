package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

type principal struct {
	Name string
	Role string
}

// SecurityMetricsSnapshot contains bounded-label management security totals.
type SecurityMetricsSnapshot struct {
	AuthenticationFailures uint64 `json:"authentication_failures"`
	AuthorizationFailures  uint64 `json:"authorization_failures"`
	RateLimitDenials       uint64 `json:"rate_limit_denials"`
	AuditEvents            uint64 `json:"audit_events"`
}

// SecurityCounters is shared by the middleware and metrics collector.
type SecurityCounters struct {
	authenticationFailures atomic.Uint64
	authorizationFailures  atomic.Uint64
	rateLimitDenials       atomic.Uint64
}

func (c *SecurityCounters) Snapshot() SecurityMetricsSnapshot {
	if c == nil {
		return SecurityMetricsSnapshot{}
	}
	return SecurityMetricsSnapshot{
		AuthenticationFailures: c.authenticationFailures.Load(),
		AuthorizationFailures:  c.authorizationFailures.Load(),
		RateLimitDenials:       c.rateLimitDenials.Load(),
	}
}

type principalContextKey struct{}

var rolePermissions = map[string]map[string]bool{
	"viewer": {
		"server:read": true, "streams:read": true, "cluster:read": true,
		"sip:read": true, "recordings:read": true, "audit:read": true,
	},
	"operator": {
		"server:read": true, "streams:read": true, "cluster:read": true,
		"sip:read": true, "recordings:read": true, "audit:read": true,
		"streams:kick": true, "sip:calls": true, "config:reload": true,
	},
	"admin": {"*": true},
}

func roleAllows(role, permission string) bool {
	permissions := rolePermissions[strings.ToLower(role)]
	return permissions["*"] || permissions[permission]
}

func defaultRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return "admin"
	}
	if _, ok := rolePermissions[role]; ok {
		return role
	}
	return "invalid"
}

func permissionForRequest(r *http.Request) string {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/debug/"):
		return "debug:read"
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/api/v1/streams/"):
		return "streams:delete"
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/api/v1/streams/") && strings.HasSuffix(p, "/kick"):
		return "streams:kick"
	case r.Method == http.MethodPost && p == "/api/v1/server/config/refresh":
		return "config:reload"
	case strings.HasPrefix(p, "/api/v1/sipgateway/calls") && r.Method != http.MethodGet:
		return "sip:calls"
	case strings.HasPrefix(p, "/api/v1/sipgateway/"):
		return "sip:read"
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/api/v1/recordings/"):
		return "recordings:delete"
	case strings.HasPrefix(p, "/api/v1/recordings") || strings.HasPrefix(p, "/api/v1/dvr"):
		return "recordings:read"
	case strings.HasPrefix(p, "/api/v1/cluster"):
		return "cluster:read"
	case strings.HasPrefix(p, "/api/v1/audit"):
		return "audit:read"
	case strings.HasPrefix(p, "/api/v1/security"):
		return "audit:read"
	case strings.HasPrefix(p, "/api/v1/streams"):
		return "streams:read"
	default:
		return "server:read"
	}
}

func resolvePrincipal(r *http.Request, cfg config.APIConfig) (principal, bool) {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimPrefix(auth, "Bearer ")
		if cfg.Auth.BearerToken != "" && secureEqual(token, cfg.Auth.BearerToken) {
			return principal{Name: "legacy-bearer", Role: "admin"}, true
		}
		for _, binding := range cfg.Auth.Tokens {
			if binding.Token != "" && secureEqual(token, binding.Token) {
				name := binding.Name
				if name == "" {
					name = "bearer"
				}
				return principal{Name: name, Role: defaultRole(binding.Role)}, true
			}
		}
	}
	if cfg.Console.Username != "" && validateSession(r, cfg.Console) {
		return principal{Name: cfg.Console.Username, Role: defaultRole(cfg.Console.Role)}, true
	}
	if cfg.Auth.BearerToken == "" && len(cfg.Auth.Tokens) == 0 {
		return principal{Name: "anonymous", Role: "admin"}, true
	}
	return principal{}, false
}

func secureEqual(got, want string) bool {
	return len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(b[:])
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func buildSecurityHandler(next http.Handler, server *core.Server, audit *AuditStore, optionalCounters ...*SecurityCounters) http.Handler {
	var counters *SecurityCounters
	if len(optionalCounters) > 0 {
		counters = optionalCounters[0]
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := server.Config().API
		if r.URL.Path == "/console/login" {
			handleSecuredLogin(w, r, cfg.Console, audit, counters)
			return
		}
		if r.URL.Path == "/api/v1/server/health" {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/console") && !strings.HasPrefix(r.URL.Path, "/console/static/") {
			if cfg.Console.Username != "" && !validateSession(r, cfg.Console) {
				http.Redirect(w, r, "/console/login", http.StatusFound)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/debug/") {
			next.ServeHTTP(w, r)
			return
		}

		requestID := newRequestID()
		permission := permissionForRequest(r)
		p, ok := resolvePrincipal(r, cfg)
		if !ok {
			if counters != nil {
				counters.authenticationFailures.Add(1)
			}
			audit.Record(AuditEntry{RequestID: requestID, Principal: "unknown", Action: permission, Resource: r.URL.Path, Result: "unauthorized", RemoteAddr: r.RemoteAddr})
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !roleAllows(p.Role, permission) {
			if counters != nil {
				counters.authorizationFailures.Add(1)
			}
			audit.Record(AuditEntry{RequestID: requestID, Principal: p.Name, Role: p.Role, Action: permission, Resource: r.URL.Path, Result: "denied", RemoteAddr: r.RemoteAddr})
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}

		ctx := context.WithValue(r.Context(), principalContextKey{}, p)
		if !isAuditedOperation(permission) {
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r.WithContext(ctx))
		result := "success"
		if sw.status >= 400 {
			result = "failed"
		}
		audit.Record(AuditEntry{RequestID: requestID, Principal: p.Name, Role: p.Role, Action: permission, Resource: r.URL.Path, Result: result, RemoteAddr: r.RemoteAddr})
	})
}

func handleSecuredLogin(w http.ResponseWriter, r *http.Request, cfg config.ConsoleConfig, audit *AuditStore, counters *SecurityCounters) {
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	handleLogin(sw, r, cfg)
	if r.Method != http.MethodPost || sw.status != http.StatusUnauthorized {
		return
	}
	if counters != nil {
		counters.authenticationFailures.Add(1)
	}
	audit.Record(AuditEntry{
		RequestID:  newRequestID(),
		Principal:  "unknown",
		Action:     "console:login",
		Resource:   "/console/login",
		Result:     "unauthorized",
		RemoteAddr: r.RemoteAddr,
	})
}

func isAuditedOperation(permission string) bool {
	switch permission {
	case "streams:delete", "streams:kick", "sip:calls", "recordings:delete", "config:reload":
		return true
	default:
		return false
	}
}
