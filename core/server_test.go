package core

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
)

type mockModule struct {
	name   string
	inited bool
	closed bool
	hooks  []HookRegistration
}

func (m *mockModule) Name() string             { return m.name }
func (m *mockModule) Init(s *Server) error      { m.inited = true; return nil }
func (m *mockModule) Hooks() []HookRegistration { return m.hooks }
func (m *mockModule) Close() error              { m.closed = true; return nil }

func TestServerModuleLifecycle(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Name = "test"
	s := NewServer(cfg)

	mod := &mockModule{name: "test-module"}
	s.RegisterModule(mod)

	if err := s.Init(); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if !mod.inited {
		t.Error("expected module to be inited")
	}

	s.Shutdown()
	if !mod.closed {
		t.Error("expected module to be closed")
	}
}

func TestServerModuleCloseReverseOrder(t *testing.T) {
	cfg := &config.Config{}
	s := NewServer(cfg)

	var order []string
	s.RegisterModule(&orderTrackModule{name: "first", order: &order})
	s.RegisterModule(&orderTrackModule{name: "second", order: &order})

	_ = s.Init()
	s.Shutdown()

	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Errorf("expected close order [second, first], got %v", order)
	}
}

type orderTrackModule struct {
	name  string
	order *[]string
}

func (m *orderTrackModule) Name() string             { return m.name }
func (m *orderTrackModule) Init(s *Server) error      { return nil }
func (m *orderTrackModule) Hooks() []HookRegistration { return nil }
func (m *orderTrackModule) Close() error              { *m.order = append(*m.order, m.name); return nil }

func TestServerStreamHub(t *testing.T) {
	cfg := &config.Config{}
	cfg.Stream.RingBufferSize = 256
	s := NewServer(cfg)
	if s.StreamHub() == nil {
		t.Fatal("expected StreamHub to be initialized")
	}
}

// generateTestCert creates a self-signed certificate in tmpDir and returns cert/key paths.
func generateTestCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	certOut, _ := os.Create(certFile)
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	certOut.Close()

	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyOut, _ := os.Create(keyFile)
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	keyOut.Close()

	return certFile, keyFile
}

// isTLSListener checks if the accepted connection from ln is a *tls.Conn.
func isTLSListener(t *testing.T, ln net.Listener) bool {
	t.Helper()

	type result struct {
		isTLS bool
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ch <- result{false}
			return
		}
		_, ok := conn.(*tls.Conn)
		conn.Close()
		ch <- result{ok}
	}()

	// Plain TCP dial is enough — the listener wraps it in TLS on accept.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()

	r := <-ch
	return r.isTLS
}

func TestMakeListenerPlainTCP(t *testing.T) {
	cfg := &config.Config{}
	s := NewServer(cfg)

	ln, err := s.MakeListener("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("MakeListener: %v", err)
	}
	defer ln.Close()

	if isTLSListener(t, ln) {
		t.Fatal("expected plain TCP listener, got TLS")
	}
}

func TestMakeListenerGlobalTLS(t *testing.T) {
	certFile, keyFile := generateTestCert(t)
	cfg := &config.Config{}
	cfg.TLS.CertFile = certFile
	cfg.TLS.KeyFile = keyFile

	s := NewServer(cfg)

	ln, err := s.MakeListener("127.0.0.1:0", nil)
	if err != nil {
		t.Fatalf("MakeListener: %v", err)
	}
	defer ln.Close()

	if !isTLSListener(t, ln) {
		t.Fatal("expected TLS listener")
	}
}

func TestMakeListenerModuleOverrideFalse(t *testing.T) {
	certFile, keyFile := generateTestCert(t)
	cfg := &config.Config{}
	cfg.TLS.CertFile = certFile
	cfg.TLS.KeyFile = keyFile

	s := NewServer(cfg)

	off := false
	ln, err := s.MakeListener("127.0.0.1:0", &off)
	if err != nil {
		t.Fatalf("MakeListener: %v", err)
	}
	defer ln.Close()

	if isTLSListener(t, ln) {
		t.Fatal("expected plain TCP listener when module TLS=false")
	}
}

func TestMakeListenerModuleOverrideTrueNoCert(t *testing.T) {
	cfg := &config.Config{} // no TLS cert configured
	s := NewServer(cfg)

	on := true
	_, err := s.MakeListener("127.0.0.1:0", &on)
	if err == nil {
		t.Fatal("expected error when module TLS=true but no cert configured")
	}
}

func TestMakeListenerModuleOverrideTrue(t *testing.T) {
	certFile, keyFile := generateTestCert(t)
	cfg := &config.Config{}
	cfg.TLS.CertFile = certFile
	cfg.TLS.KeyFile = keyFile

	s := NewServer(cfg)

	on := true
	ln, err := s.MakeListener("127.0.0.1:0", &on)
	if err != nil {
		t.Fatalf("MakeListener: %v", err)
	}
	defer ln.Close()

	if !isTLSListener(t, ln) {
		t.Fatal("expected TLS listener")
	}
}

func TestServerAccessors(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Name = "accessors-test"
	s := NewServer(cfg)

	// Config
	if s.Config() != cfg {
		t.Error("Config() should return the same config pointer")
	}

	// EventBus
	if s.GetEventBus() == nil {
		t.Error("GetEventBus() should not be nil")
	}

	// StartTime
	if s.StartTime().IsZero() {
		t.Error("StartTime() should not be zero")
	}

	// UptimeSeconds
	if s.UptimeSeconds() < 0 {
		t.Error("UptimeSeconds() should be non-negative")
	}
}

func TestServerModuleNames(t *testing.T) {
	cfg := &config.Config{}
	s := NewServer(cfg)

	s.RegisterModule(&mockModule{name: "rtmp"})
	s.RegisterModule(&mockModule{name: "hls"})
	_ = s.Init()
	defer s.Shutdown()

	names := s.ModuleNames()
	if len(names) != 2 {
		t.Fatalf("expected 2 module names, got %d", len(names))
	}
	if names[0] != "rtmp" || names[1] != "hls" {
		t.Errorf("unexpected module names: %v", names)
	}
}

func TestServerAPIHandlers(t *testing.T) {
	cfg := &config.Config{}
	s := NewServer(cfg)

	handler := http.NewServeMux()
	s.RegisterAPIHandler("/test", handler)

	handlers := s.APIHandlers()
	if len(handlers) != 1 {
		t.Fatalf("expected 1 API handler, got %d", len(handlers))
	}
	if handlers["/test"] == nil {
		t.Error("expected handler for /test")
	}
}

func TestServerConnectionTracking(t *testing.T) {
	cfg := &config.Config{}
	cfg.Limits.MaxConnections = 2
	s := NewServer(cfg)

	if s.ConnectionCount() != 0 {
		t.Errorf("expected 0 connections, got %d", s.ConnectionCount())
	}

	if !s.AcquireConn() {
		t.Error("first AcquireConn should succeed")
	}
	if s.ConnectionCount() != 1 {
		t.Errorf("expected 1 connection, got %d", s.ConnectionCount())
	}

	if !s.AcquireConn() {
		t.Error("second AcquireConn should succeed")
	}
	if s.ConnectionCount() != 2 {
		t.Errorf("expected 2 connections, got %d", s.ConnectionCount())
	}

	// Third should fail (max=2)
	if s.AcquireConn() {
		t.Error("third AcquireConn should fail (max=2)")
	}

	s.ReleaseConn()
	if s.ConnectionCount() != 1 {
		t.Errorf("expected 1 connection after release, got %d", s.ConnectionCount())
	}

	// Should be able to acquire again
	if !s.AcquireConn() {
		t.Error("AcquireConn should succeed after release")
	}
}

func TestServerConnectionTrackingUnlimited(t *testing.T) {
	cfg := &config.Config{} // MaxConnections = 0 (unlimited)
	s := NewServer(cfg)

	for i := 0; i < 100; i++ {
		if !s.AcquireConn() {
			t.Fatalf("AcquireConn should always succeed with no limit, failed at %d", i)
		}
	}
	if s.ConnectionCount() != 100 {
		t.Errorf("expected 100 connections, got %d", s.ConnectionCount())
	}
}

func TestTLSConfigConfigured(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.TLSConfig
		expected bool
	}{
		{"empty", config.TLSConfig{}, false},
		{"cert only", config.TLSConfig{CertFile: "cert.pem"}, false},
		{"key only", config.TLSConfig{KeyFile: "key.pem"}, false},
		{"both", config.TLSConfig{CertFile: "cert.pem", KeyFile: "key.pem"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Configured(); got != tt.expected {
				t.Errorf("Configured() = %v, want %v", got, tt.expected)
			}
		})
	}
}
