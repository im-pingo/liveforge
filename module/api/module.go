package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

// Module implements the management API as a standalone HTTP server.
type Module struct {
	listener net.Listener
	httpSrv  *http.Server
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

	ln, err := s.MakeListener(cfg.API.Listen, cfg.API.TLS)
	if err != nil {
		return err
	}
	m.listener = ln

	mux := http.NewServeMux()
	RegisterRoutes(mux, s)

	handler := buildAuthHandler(mux, cfg.API)
	m.httpSrv = &http.Server{Handler: handler}

	proto := "http"
	if cfg.TLS.Configured() && (cfg.API.TLS == nil || *cfg.API.TLS) {
		proto = "https"
	}
	log.Printf("[api] %s listening on %s", proto, ln.Addr())

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[api] serve error: %v", err)
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
	m.wg.Wait()
	log.Println("[api] stopped")
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
			handleLogin(w, r, cfg.Console)
			return
		}

		// API endpoints — Bearer Token OR valid session cookie
		if strings.HasPrefix(r.URL.Path, "/api/") {
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
func handleLogin(w http.ResponseWriter, r *http.Request, cfg config.ConsoleConfig) {
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

	if username != cfg.Username || password != cfg.Password {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write(loginFailHTML)
		return
	}

	token := generateSessionToken(cfg)
	http.SetCookie(w, &http.Cookie{
		Name:     "lf_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400, // 24h
	})
	http.Redirect(w, r, "/console", http.StatusSeeOther)
}
