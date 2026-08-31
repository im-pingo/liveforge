package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/config"
	configruntime "github.com/im-pingo/liveforge/config/runtime"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/ratelimit"
)

// Module implements the management API as a standalone HTTP server.
type Module struct {
	listener  net.Listener
	httpSrv   *http.Server
	server    *core.Server
	limiter   *ratelimit.Limiter
	limiterMu sync.RWMutex
	rateCfg   config.RateLimitConfig
	audit     *AuditStore
	security  SecurityCounters
	wg        sync.WaitGroup
}

// NewModule creates a new API module.
func NewModule() *Module {
	return &Module{}
}

// Name returns the module name.
func (m *Module) Name() string { return "api" }

// Init initializes the standalone API HTTP server.
func (m *Module) Init(s *core.Server) error {
	cfg := s.Config()
	m.server = s
	m.audit = NewAuditStore(cfg.API.Audit.MaxEntries)

	ln, err := s.MakeListenerAutoTLS(cfg.API.Listen, cfg.API.TLS)
	if err != nil {
		return err
	}
	m.listener = ln

	mux := http.NewServeMux()
	registerRoutes(mux, s, m.audit)

	var handler http.Handler = buildSecurityHandler(mux, s, m.audit, &m.security)
	if rl := cfg.Limits.RateLimit; rl.Enabled && rl.Rate > 0 {
		m.limiter = ratelimit.NewWithTrustedProxies(rl.Rate, rl.Burst, rl.TrustedProxies)
	}
	m.rateCfg = cfg.Limits.RateLimit
	m.httpSrv = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.limiterMu.RLock()
			limiter := m.limiter
			m.limiterMu.RUnlock()
			if limiter != nil && !limiter.AllowRequest(r) {
				m.security.rateLimitDenials.Add(1)
				m.auditRateLimitDenial(r)
				writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
			handler.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	proto := "http"
	if s.HasTLS() && (cfg.API.TLS == nil || *cfg.API.TLS) {
		proto = "https"
	}
	slog.Info("listening", "module", "api", "proto", proto, "addr", ln.Addr())

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("serve error", "module", "api", "error", err)
		}
	}()

	return nil
}

func (m *Module) auditRateLimitDenial(r *http.Request) {
	permission := permissionForRequest(r)
	if m.audit == nil || !isAuditedOperation(permission) {
		return
	}
	p, ok := resolvePrincipal(r, m.server.Config().API)
	if !ok {
		p = principal{Name: "unknown"}
	}
	m.audit.Record(AuditEntry{
		RequestID:  newRequestID(),
		Principal:  p.Name,
		Role:       p.Role,
		Action:     permission,
		Resource:   r.URL.Path,
		Result:     "failed",
		RemoteAddr: r.RemoteAddr,
	})
}

// SecurityMetrics returns bounded-label management security counters.
func (m *Module) SecurityMetrics() SecurityMetricsSnapshot {
	snapshot := m.security.Snapshot()
	if m.audit != nil {
		snapshot.AuditEvents = m.audit.Total()
	}
	return snapshot
}

// SecurityMetricValues exposes a fixed key set to the metrics module without
// coupling it to API implementation types.
func (m *Module) SecurityMetricValues() map[string]float64 {
	snapshot := m.SecurityMetrics()
	return map[string]float64{
		"authentication_failures": float64(snapshot.AuthenticationFailures),
		"authorization_failures":  float64(snapshot.AuthorizationFailures),
		"rate_limit_denials":      float64(snapshot.RateLimitDenials),
		"audit_events":            float64(snapshot.AuditEvents),
	}
}

// OnReload updates API authentication through the server snapshot read path
// and replaces the per-IP rate limiter for subsequent requests.
func (m *Module) OnReload(s *core.Server) error {
	rl := s.Config().Limits.RateLimit
	m.limiterMu.Lock()
	previous := m.rateCfg
	m.rateCfg = rl
	m.limiterMu.Unlock()
	if !reflect.DeepEqual(previous, rl) {
		var next *ratelimit.Limiter
		if rl.Enabled && rl.Rate > 0 {
			next = ratelimit.NewWithTrustedProxies(rl.Rate, rl.Burst, rl.TrustedProxies)
		}
		m.limiterMu.Lock()
		old := m.limiter
		m.limiter = next
		m.limiterMu.Unlock()
		if old != nil {
			old.Close()
		}
	}
	return nil
}

// OnConfigApplied records success only after the server has committed the
// candidate and every reloadable module has accepted it.
func (m *Module) OnConfigApplied(snapshot *configruntime.ConfigSnapshot) {
	if m.audit == nil || snapshot == nil {
		return
	}
	m.audit.Record(AuditEntry{
		Principal: "config-source:" + snapshot.Source,
		Role:      "system",
		Action:    "config:apply",
		Resource:  snapshot.Version.Value,
		Result:    "success",
	})
}

// Hooks returns the module's event hooks (none for the API module).
func (m *Module) Hooks() []core.HookRegistration { return nil }

// Close shuts down the API server.
func (m *Module) Close() error {
	if m.httpSrv != nil {
		m.httpSrv.Close()
	}
	m.limiterMu.Lock()
	limiter := m.limiter
	m.limiter = nil
	m.limiterMu.Unlock()
	if limiter != nil {
		limiter.Close()
	}
	m.wg.Wait()
	slog.Info("stopped", "module", "api")
	return nil
}

// Addr returns the listener address (useful for tests).
func (m *Module) Addr() net.Addr {
	if m.listener != nil {
		return m.listener.Addr()
	}
	return nil
}

// buildAuthHandler wraps the mux with authentication middleware.
// - API endpoints (/api/*) are protected by Bearer Token (if configured)
// - Console (/console) is protected by session cookie login (if console credentials configured)
// - Login endpoint (/console/login) is always accessible
func buildAuthHandler(mux *http.ServeMux, cfg config.APIConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Login endpoint — always accessible
		if r.URL.Path == "/console/login" {
			handleLogin(w, r, cfg.Console, false)
			return
		}

		// API and debug endpoints — Bearer Token OR valid session cookie
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/debug/") {
			// Keep health probes available without credentials.
			if r.URL.Path == "/api/v1/server/health" {
				mux.ServeHTTP(w, r)
				return
			}
			if token := cfg.Auth.BearerToken; token != "" {
				auth := r.Header.Get("Authorization")
				hasBearerToken := strings.HasPrefix(auth, "Bearer ") && auth[7:] == token
				hasSession := cfg.Console.Username != "" && validateSession(r, cfg.Console)
				if !hasBearerToken && !hasSession {
					writeError(w, http.StatusUnauthorized, "unauthorized")
					return
				}
			}
			mux.ServeHTTP(w, r)
			return
		}

		// Console — session cookie auth (if credentials configured)
		if strings.HasPrefix(r.URL.Path, "/console") {
			if cfg.Console.Username != "" {
				if !validateSession(r, cfg.Console) {
					loginURL := "/console/login"
					if r.URL.Path == "/console/publish" {
						loginURL += "?redirect=" + url.QueryEscape(r.URL.Path)
					}
					http.Redirect(w, r, loginURL, http.StatusFound)
					return
				}
			}
			mux.ServeHTTP(w, r)
			return
		}

		mux.ServeHTTP(w, r)
	})
}

// sessionSecret derives a signing key from the console password.
func sessionSecret(cfg config.ConsoleConfig) []byte {
	h := sha256.Sum256([]byte("liveforge-session:" + cfg.Password))
	return h[:]
}

// generateSessionToken creates a signed session token.
func generateSessionToken(cfg config.ConsoleConfig) string {
	// Token = expiry_hex + "." + hmac_hex
	expiry := time.Now().Add(24 * time.Hour).Unix()
	payload := fmt.Sprintf("%x", expiry)
	mac := hmac.New(sha256.New, sessionSecret(cfg))
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return payload + "." + sig
}

// validateSession checks the session cookie.
func validateSession(r *http.Request, cfg config.ConsoleConfig) bool {
	cookie, err := r.Cookie("lf_session")
	if err != nil {
		return false
	}
	parts := strings.SplitN(cookie.Value, ".", 2)
	if len(parts) != 2 {
		return false
	}
	// Verify signature
	mac := hmac.New(sha256.New, sessionSecret(cfg))
	mac.Write([]byte(parts[0]))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[1]), []byte(expected)) {
		return false
	}
	// Check expiry
	var expiry int64
	fmt.Sscanf(parts[0], "%x", &expiry)
	return time.Now().Unix() < expiry
}

// handleLogin serves the login page (GET) or processes login (POST).
func handleLogin(w http.ResponseWriter, r *http.Request, cfg config.ConsoleConfig, secure bool) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		redirect := "/console"
		if r.URL.Query().Get("redirect") == "/console/publish" {
			redirect = "/console/publish"
		}
		if _, err := w.Write(renderLoginPage(loginHTML, redirect)); err != nil {
			return
		}
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.ParseForm()
	username := r.FormValue("username")
	password := r.FormValue("password")
	redirect := "/console"
	if r.FormValue("redirect") == "/console/publish" {
		redirect = "/console/publish"
	}

	if !secureEqual(username, cfg.Username) || !secureEqual(password, cfg.Password) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		if _, err := w.Write(renderLoginPage(loginFailHTML, redirect)); err != nil {
			return
		}
		return
	}

	token := generateSessionToken(cfg)
	http.SetCookie(w, &http.Cookie{
		Name:     "lf_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400, // 24h
	})
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// consoleLoginRedirect keeps login return targets local to the console. Only
// the two supported shells are accepted; all other values fall back safely.
func consoleLoginRedirect(raw string) string {
	if raw == "/console/publish" {
		return raw
	}
	return "/console"
}

func renderLoginPage(page []byte, redirect string) []byte {
	marker := []byte(`name="redirect" value=""`)
	replacement := []byte(`name="redirect" value="` + html.EscapeString(consoleLoginRedirect(redirect)) + `"`)
	return bytes.Replace(page, marker, replacement, 1)
}
