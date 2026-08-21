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
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// Server is the main application server that manages modules and lifecycle.
type Server struct {
	configPtr  atomic.Pointer[config.Config]
	authorizer atomic.Pointer[authorizerHolder]
	eventBus   *EventBus
	hub        *StreamHub
	modules    []Module
	startTime  time.Time
	connCount  atomic.Int64
	done       chan struct{}

	apiMu         sync.RWMutex
	apiHandlers   map[string]http.Handler
	configMu      sync.RWMutex
	configUpdater ConfigUpdater

	autoCertOnce sync.Once
	autoCert     *tls.Certificate // auto-generated self-signed cert (nil if file-based TLS configured)
}

// ConfigUpdater exposes cached configuration status and revision-checked updates.
type ConfigUpdater interface {
	Current() config.Snapshot
	Update(context.Context, config.Patch, string) (config.ApplyResult, error)
}

// NewServer creates a new Server instance.
func NewServer(cfg *config.Config) *Server {
	ownedConfig := config.CloneConfig(cfg)
	bus := NewEventBus()
	s := &Server{
		eventBus:    bus,
		hub:         NewStreamHub(ownedConfig.Stream, ownedConfig.Limits, bus),
		startTime:   time.Now(),
		done:        make(chan struct{}),
		apiHandlers: make(map[string]http.Handler),
	}
	s.configPtr.Store(ownedConfig)
	return s
}

// Config returns the immutable server-owned configuration snapshot. Callers
// that need to modify configuration must clone it and call ApplyConfig.
func (s *Server) Config() *config.Config {
	return s.configPtr.Load()
}

// SetConfigUpdater connects the server to its runtime configuration manager.
func (s *Server) SetConfigUpdater(updater ConfigUpdater) {
	s.configMu.Lock()
	s.configUpdater = updater
	s.configMu.Unlock()
}

// ConfigUpdater returns the runtime configuration manager, if configured.
func (s *Server) ConfigUpdater() ConfigUpdater {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.configUpdater
}

// ApplyConfig validates a runtime configuration change with every affected
// module before publishing it. Once published, applying the prepared change
// must not fail.
func (s *Server) ApplyConfig(cfg *config.Config) error {
	current := s.Config()
	next := config.CloneConfig(cfg)
	for _, m := range s.modules {
		if configurable, ok := m.(Configurable); ok {
			if err := configurable.ValidateConfigChange(config.CloneConfig(current), config.CloneConfig(next)); err != nil {
				return fmt.Errorf("validate config for module %s: %w", m.Name(), err)
			}
		}
	}

	s.configPtr.Store(next)
	for _, m := range s.modules {
		if configurable, ok := m.(Configurable); ok {
			configurable.ApplyConfigChange(config.CloneConfig(current), config.CloneConfig(next))
		}
	}
	return nil
}

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
	s.modules = append(s.modules, m)
}

// Init initializes all registered modules, registers their hooks, and starts the alive loop.
func (s *Server) Init() error {
	for _, m := range s.modules {
		if err := m.Init(s); err != nil {
			return err
		}
		for _, h := range m.Hooks() {
			s.eventBus.Register(h)
		}
	}

	go s.aliveLoop()

	return nil
}

// Shutdown stops the alive loop and closes all modules in reverse registration order.
func (s *Server) Shutdown() {
	close(s.done)
	for i := len(s.modules) - 1; i >= 0; i-- {
		s.modules[i].Close() //nolint:errcheck
	}
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
	names := make([]string, len(s.modules))
	for i, m := range s.modules {
		names[i] = m.Name()
	}
	return names
}

// ModuleByName returns the module with the given name, or nil if not found.
func (s *Server) ModuleByName(name string) Module {
	for _, m := range s.modules {
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
	for {
		current := s.connCount.Load()
		if max > 0 && current >= int64(max) {
			return false
		}
		if s.connCount.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// ReleaseConn decrements the connection counter.
func (s *Server) ReleaseConn() {
	s.connCount.Add(-1)
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
