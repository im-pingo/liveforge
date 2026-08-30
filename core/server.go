package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
	configruntime "github.com/im-pingo/liveforge/config/runtime"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// Server is the main application server that manages modules and lifecycle.
type Server struct {
	configPtr     atomic.Pointer[config.Config]
	authorizer    atomic.Pointer[authorizerHolder]
	eventBus      *EventBus
	hub           *StreamHub
	modules       []Module
	attempted     []Module
	modulesMu     sync.RWMutex
	configApplyMu sync.Mutex
	startTime     time.Time
	connCount     atomic.Int64
	done          chan struct{}
	shutdownOnce  sync.Once
	aliveWG       sync.WaitGroup

	apiMu          sync.RWMutex
	apiHandlers    map[string]http.Handler
	pendingMu      sync.RWMutex
	pendingRestart []string
	configManager  *configruntime.Manager

	autoCertOnce sync.Once
	autoCert     *tls.Certificate // auto-generated self-signed cert (nil if file-based TLS configured)
}

// NewServer creates a new Server instance.
func NewServer(cfg *config.Config) *Server {
	bus := NewEventBus()
	s := &Server{
		eventBus:    bus,
		hub:         NewStreamHub(cfg.Stream, cfg.Limits, bus),
		startTime:   time.Now(),
		done:        make(chan struct{}),
		apiHandlers: make(map[string]http.Handler),
	}
	s.configPtr.Store(cfg)
	return s
}

// Config returns the server configuration.
func (s *Server) Config() *config.Config {
	return s.configPtr.Load()
}

// UpdateConfig atomically swaps the server configuration and notifies all
// Reloadable modules. Errors from individual modules are logged but do not
// stop the reload process.
func (s *Server) UpdateConfig(cfg *config.Config) {
	_ = s.updateConfig(cfg, nil)
}

// UpdateConfigSnapshot publishes a validated runtime snapshot and records
// changes that require a process restart. Existing module reload callbacks are
// still invoked for the hot portion of the snapshot.
func (s *Server) UpdateConfigSnapshot(snapshot *configruntime.ConfigSnapshot) error {
	if snapshot == nil || snapshot.Config == nil {
		return nil
	}
	if err := s.updateConfig(snapshot.Config, snapshot.PendingRestart); err != nil {
		return err
	}
	for _, m := range s.moduleSnapshot() {
		if listener, ok := m.(ConfigApplied); ok {
			listener.OnConfigApplied(snapshot)
		}
	}
	return nil
}

func (s *Server) updateConfig(cfg *config.Config, pending []string) error {
	s.configApplyMu.Lock()
	defer s.configApplyMu.Unlock()

	view := s.reloadView(cfg)
	modules := s.moduleSnapshot()
	prepared := make(map[int]func())
	for i, m := range modules {
		if _, ok := m.(Reloadable); !ok {
			continue
		}
		if preparer, ok := m.(ReloadPreparer); ok {
			commit, err := preparer.PrepareReload(view)
			if err != nil {
				return fmt.Errorf("%s: %w", m.Name(), err)
			}
			prepared[i] = commit
		}
	}
	applied := make([]Reloadable, 0, len(modules))
	var reloadErrors []error
	for i, m := range modules {
		if r, ok := m.(Reloadable); ok {
			applied = append(applied, r)
			if commit, preparedOK := prepared[i]; preparedOK {
				if commit != nil {
					commit()
				}
				continue
			}
			if err := r.OnReload(view); err != nil {
				slog.Error("module reload failed", "module", m.Name(), "error", err)
				reloadErrors = append(reloadErrors, fmt.Errorf("%s: %w", m.Name(), err))
				break
			}
		}
	}
	if err := errors.Join(reloadErrors...); err != nil {
		for i := len(applied) - 1; i >= 0; i-- {
			if rollbackErr := applied[i].OnReload(s); rollbackErr != nil {
				slog.Error("module reload rollback failed", "error", rollbackErr)
			}
		}
		return err
	}

	s.hub.UpdatePolicy(cfg.Stream, cfg.Limits)
	s.pendingMu.Lock()
	s.pendingRestart = append([]string(nil), pending...)
	s.pendingMu.Unlock()
	s.configPtr.Store(cfg)
	return nil
}

func (s *Server) reloadView(cfg *config.Config) *Server {
	view := &Server{
		eventBus:      s.eventBus,
		hub:           s.hub,
		startTime:     s.startTime,
		done:          s.done,
		configManager: s.configManager,
	}
	view.configPtr.Store(cfg)
	return view
}

func (s *Server) moduleSnapshot() []Module {
	s.modulesMu.RLock()
	defer s.modulesMu.RUnlock()
	return append([]Module(nil), s.modules...)
}

func (s *Server) attemptedModuleSnapshot() []Module {
	s.modulesMu.RLock()
	defer s.modulesMu.RUnlock()
	return append([]Module(nil), s.attempted...)
}

// PendingRestartChanges returns configuration paths that are active in the
// published snapshot but cannot be applied to listeners/modules in place.
func (s *Server) PendingRestartChanges() []string {
	s.pendingMu.RLock()
	defer s.pendingMu.RUnlock()
	return append([]string(nil), s.pendingRestart...)
}

// SetConfigManager attaches the runtime manager for status/API integrations.
func (s *Server) SetConfigManager(manager *configruntime.Manager) { s.configManager = manager }

// ConfigManager returns the attached runtime manager, if any.
func (s *Server) ConfigManager() *configruntime.Manager { return s.configManager }

// GetEventBus returns the server's event bus.
func (s *Server) GetEventBus() *EventBus {
	return s.eventBus
}

// StreamHub returns the shared stream hub.
func (s *Server) StreamHub() *StreamHub {
	return s.hub
}

// RegisterModule adds a module to the server.
func (s *Server) RegisterModule(m Module) {
	s.modulesMu.Lock()
	defer s.modulesMu.Unlock()
	s.modules = append(s.modules, m)
}

// Init initializes all registered modules, registers their hooks, and starts the alive loop.
func (s *Server) Init() error {
	for _, m := range s.moduleSnapshot() {
		s.modulesMu.Lock()
		s.attempted = append(s.attempted, m)
		s.modulesMu.Unlock()
		if err := m.Init(s); err != nil {
			return err
		}
		for _, h := range m.Hooks() {
			s.eventBus.Register(h)
		}
	}

	s.aliveWG.Add(1)
	go func() {
		defer s.aliveWG.Done()
		s.aliveLoop()
	}()

	return nil
}

// Shutdown stops the alive loop and closes all modules in reverse registration order.
func (s *Server) Shutdown() {
	s.shutdownOnce.Do(func() {
		close(s.done)
		s.aliveWG.Wait()
		modules := s.attemptedModuleSnapshot()
		for i := len(modules) - 1; i >= 0; i-- {
			modules[i].Close() //nolint:errcheck
		}

		timeout := s.Config().Server.DrainTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := s.eventBus.Drain(ctx); err != nil {
			slog.Error("event bus drain timed out during shutdown", "timeout", timeout, "error", err)
		}
	})
}

// StartTime returns when the server was created.
func (s *Server) StartTime() time.Time {
	return s.startTime
}

// UptimeSeconds returns the server uptime in seconds.
func (s *Server) UptimeSeconds() float64 {
	return time.Since(s.startTime).Seconds()
}

// ModuleNames returns the names of all registered modules.
func (s *Server) ModuleNames() []string {
	modules := s.moduleSnapshot()
	names := make([]string, len(modules))
	for i, m := range modules {
		names[i] = m.Name()
	}
	return names
}

// ModuleByName returns the module with the given name, or nil if not found.
func (s *Server) ModuleByName(name string) Module {
	for _, m := range s.moduleSnapshot() {
		if m.Name() == name {
			return m
		}
	}
	return nil
}

// RegisterAPIHandler registers an HTTP handler for the given pattern on the API mux.
// Modules call this during Init to expose HTTP/WebSocket endpoints on the API server.
func (s *Server) RegisterAPIHandler(pattern string, h http.Handler) {
	s.apiMu.Lock()
	defer s.apiMu.Unlock()
	s.apiHandlers[pattern] = h
}

// APIHandlers returns a copy of all registered API handlers.
func (s *Server) APIHandlers() map[string]http.Handler {
	s.apiMu.RLock()
	defer s.apiMu.RUnlock()
	out := make(map[string]http.Handler, len(s.apiHandlers))
	for k, v := range s.apiHandlers {
		out[k] = v
	}
	return out
}

// AcquireConn increments the connection counter. Returns false if max_connections is exceeded.
func (s *Server) AcquireConn() bool {
	max := s.Config().Limits.MaxConnections
	if max <= 0 {
		s.connCount.Add(1)
		return true
	}

	limit := int64(max)
	for {
		current := s.connCount.Load()
		if current >= limit {
			return false
		}
		if s.connCount.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// ReleaseConn decrements the connection counter.
func (s *Server) ReleaseConn() {
	for {
		current := s.connCount.Load()
		if current <= 0 {
			return
		}
		if s.connCount.CompareAndSwap(current, current-1) {
			return
		}
	}
}

// ConnectionCount returns the current number of active connections.
func (s *Server) ConnectionCount() int64 {
	return s.connCount.Load()
}

// MakeListener creates a TCP listener on addr with optional TLS.
//
// The moduleTLS parameter is the per-module TLS override (*bool):
//   - nil  → follow global TLS config (use TLS if cert/key are configured)
//   - true → force TLS on (error if global cert/key not configured)
//   - false → force TLS off (plain TCP even if global cert/key are configured)
func (s *Server) MakeListener(addr string, moduleTLS *bool) (net.Listener, error) {
	useTLS := s.Config().TLS.Configured() // default: follow global
	if moduleTLS != nil {
		useTLS = *moduleTLS
	}

	if useTLS {
		if !s.Config().TLS.Configured() {
			return nil, fmt.Errorf("TLS enabled but tls.cert_file and tls.key_file are not configured")
		}
		cert, err := tls.LoadX509KeyPair(s.Config().TLS.CertFile, s.Config().TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load TLS certificate: %w", err)
		}
		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		return tls.Listen("tcp", addr, tlsCfg)
	}

	return net.Listen("tcp", addr)
}

// MakeListenerAutoTLS creates a TCP listener that uses TLS when available.
//
// TLS is used when any of these conditions are met:
//   - File-based cert/key are configured (tls.cert_file + tls.key_file)
//   - Auto-generated self-signed cert is enabled (tls.auto: true)
//
// If neither is configured, falls back to plain TCP.
// If the per-module TLS override is explicitly false, always uses plain TCP.
func (s *Server) MakeListenerAutoTLS(addr string, moduleTLS *bool) (net.Listener, error) {
	// If module explicitly disables TLS, use plain TCP.
	if moduleTLS != nil && !*moduleTLS {
		return net.Listen("tcp", addr)
	}

	// If file-based TLS is configured, use it.
	if s.Config().TLS.Configured() {
		cert, err := tls.LoadX509KeyPair(s.Config().TLS.CertFile, s.Config().TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load TLS certificate: %w", err)
		}
		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		return tls.Listen("tcp", addr, tlsCfg)
	}

	// Auto-generate self-signed cert only when tls.auto is enabled.
	if s.Config().TLS.Auto {
		autoCert := s.getOrCreateAutoCert()
		if autoCert == nil {
			return nil, fmt.Errorf("failed to generate self-signed TLS certificate")
		}
		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{*autoCert},
			MinVersion:   tls.VersionTLS12,
		}
		return tls.Listen("tcp", addr, tlsCfg)
	}

	// No TLS configured — plain TCP.
	return net.Listen("tcp", addr)
}

// HasTLS returns true if TLS is available (either file-based or auto-generated).
func (s *Server) HasTLS() bool {
	if s.Config().TLS.Configured() {
		return true
	}
	return s.Config().TLS.Auto && s.getOrCreateAutoCert() != nil
}

// AutoCertPEM returns the auto-generated certificate in PEM format, or nil
// if no auto-cert exists (file-based TLS is configured or cert generation failed).
func (s *Server) AutoCertPEM() []byte {
	cert := s.getOrCreateAutoCert()
	if cert == nil || len(cert.Certificate) == 0 {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Certificate[0],
	})
}

// getOrCreateAutoCert lazily generates a self-signed TLS certificate.
func (s *Server) getOrCreateAutoCert() *tls.Certificate {
	s.autoCertOnce.Do(func() {
		cert, err := generateSelfSignedCert()
		if err != nil {
			slog.Error("failed to generate self-signed TLS certificate", "error", err)
			return
		}
		slog.Info("generated self-signed TLS certificate for console HTTPS")
		s.autoCert = cert
	})
	return s.autoCert
}

// generateSelfSignedCert creates a self-signed ECDSA certificate valid for
// localhost and common LAN addresses.
func generateSelfSignedCert() (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{Organization: []string{"LiveForge"}, CommonName: "LiveForge Console"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses: []net.IP{
			net.IPv4(127, 0, 0, 1),
			net.IPv6loopback,
		},
	}

	// Add all local interface IPs as SANs so LAN access works.
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipNet, ok := a.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
				template.IPAddresses = append(template.IPAddresses, ipNet.IP)
			}
		}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}

	cert := &tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
	}
	return cert, nil
}

// aliveLoop periodically emits alive events for all active streams.
func (s *Server) aliveLoop() {
	interval := s.Config().Notify.AliveInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.emitAliveEvents()
		case <-s.done:
			return
		}
	}
}

func (s *Server) emitAliveEvents() {
	keys := s.hub.Keys()
	for _, key := range keys {
		stream, ok := s.hub.Find(key)
		if !ok || stream.State() == StreamStateDestroying {
			continue
		}

		stats := stream.Stats()
		extra := map[string]any{
			"bytes_in":     stats.BytesIn,
			"video_frames": stats.VideoFrames,
			"audio_frames": stats.AudioFrames,
			"bitrate_kbps": stats.BitrateKbps,
			"fps":          stats.FPS,
			"uptime_sec":   int64(stats.Uptime.Seconds()),
		}

		ctx := &EventContext{StreamKey: key, Extra: extra}

		s.eventBus.Emit(EventStreamAlive, ctx) //nolint:errcheck

		if stream.Publisher() != nil {
			s.eventBus.Emit(EventPublishAlive, ctx) //nolint:errcheck
		}

		subs := stream.Subscribers()
		if len(subs) > 0 {
			s.eventBus.Emit(EventSubscribeAlive, ctx) //nolint:errcheck
		}
	}
}
