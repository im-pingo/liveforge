package api

import (
	"crypto/hmac"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/ratelimit"
	"golang.org/x/crypto/bcrypt"
)

// Module implements the management API as a standalone HTTP server.
type Module struct {
	listener net.Listener
	httpSrv  *http.Server
	limiter  *ratelimit.Limiter
	wg       sync.WaitGroup
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
	sessions, err := newSessionManager()
	if err != nil {
		return err
	}
	ln, err := s.MakeListenerAutoTLS(cfg.API.Listen, cfg.API.TLS)
	if err != nil {
		return err
	}
	m.listener = ln

	var handler http.Handler = securedRoutes(s, sessions)
	if rl := cfg.Limits.RateLimit; rl.Enabled && rl.Rate > 0 {
		m.limiter = ratelimit.New(rl.Rate, rl.Burst)
		handler = m.limiter.Wrap(handler)
	}
	m.httpSrv = &http.Server{
		Handler: handler, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
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

// Hooks returns the module's event hooks (none for the API module).
func (m *Module) Hooks() []core.HookRegistration { return nil }

// Close shuts down the API server.
func (m *Module) Close() error {
	if m.httpSrv != nil {
		m.httpSrv.Close()
	}
	if m.limiter != nil {
		m.limiter.Close()
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
	return buildAuthHandlerWithConfig(mux, func() config.APIConfig { return cfg }, mustNewSessionManager())
}

func buildDynamicAuthHandler(mux *http.ServeMux, server *core.Server, sessions *sessionManager) http.Handler {
	return buildAuthHandlerWithConfig(mux, func() config.APIConfig {
		cfg := server.RuntimeConfig().API()
		return config.APIConfig{PprofEnabled: cfg.PprofEnabled, Auth: cfg.Auth, Console: cfg.Console}
	}, sessions)
}

func buildAuthHandlerWithConfig(mux *http.ServeMux, current func() config.APIConfig, sessions *sessionManager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credentialGeneration := sessions.generationSnapshot()
		cfg := current()
		if strings.HasPrefix(r.URL.Path, "/debug/pprof") && !cfg.PprofEnabled {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/server/health" {
			mux.ServeHTTP(w, r)
			return
		}
		// Login endpoint — always accessible
		if r.URL.Path == "/console/login" {
			handleLogin(w, r, cfg.Console, sessions, credentialGeneration)
			return
		}

		// API and debug endpoints — Bearer Token OR valid session cookie
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/debug/") {
			settingsEndpoint := r.URL.Path == "/api/v1/config" || strings.HasPrefix(r.URL.Path, "/api/v1/config/")
			if settingsEndpoint || cfg.Auth.Enabled {
				token := cfg.Auth.BearerToken
				auth := r.Header.Get("Authorization")
				hasBearerToken := token != "" && strings.HasPrefix(auth, "Bearer ") &&
					hmac.Equal([]byte(auth[7:]), []byte(token))
				hasSession := cfg.Console.Username != "" && sessions.validate(r, cfg.Console)
				if !hasBearerToken && !hasSession {
					writeError(w, http.StatusUnauthorized, "unauthorized")
					return
				}
				if settingsEndpoint && r.Method != http.MethodGet && !hasBearerToken &&
					(!sameOrigin(r) || !sessions.validCSRF(r, cfg.Console)) {
					writeError(w, http.StatusForbidden, "same-origin request with a valid CSRF token is required")
					return
				}
			}
			mux.ServeHTTP(w, r)
			return
		}

		// Console — session cookie auth (if credentials configured)
		if strings.HasPrefix(r.URL.Path, "/console") {
			if cfg.Console.Username != "" {
				if !sessions.validate(r, cfg.Console) {
					http.Redirect(w, r, "/console/login", http.StatusFound)
					return
				}
			}
			mux.ServeHTTP(w, r)
			return
		}

		mux.ServeHTTP(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Scheme == "" || origin.Host == "" || origin.User != nil ||
		origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return origin.Scheme == scheme && strings.EqualFold(origin.Host, r.Host)
}

// handleLogin serves the login page (GET) or processes login (POST).
func handleLogin(w http.ResponseWriter, r *http.Request, cfg config.ConsoleConfig, sessions *sessionManager, credentialGeneration uint64) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(loginHTML)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.ParseForm()
	username := r.FormValue("username")
	password := r.FormValue("password")
	if username != cfg.Username || !verifyConsolePassword(cfg, password) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write(loginFailHTML)
		return
	}

	token, ok := sessions.generateTokenFor(credentialGeneration, cfg)
	if !ok {
		writeError(w, http.StatusUnauthorized, "console credentials changed; sign in again")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "lf_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		MaxAge:   86400, // 24h
	})
	http.Redirect(w, r, "/console", http.StatusSeeOther)
}

func verifyConsolePassword(cfg config.ConsoleConfig, password string) bool {
	if cfg.PasswordHash != "" {
		return bcrypt.CompareHashAndPassword([]byte(cfg.PasswordHash), []byte(password)) == nil
	}
	if cfg.Password == "" {
		return false
	}
	return hmac.Equal([]byte(cfg.Password), []byte(password))
}
