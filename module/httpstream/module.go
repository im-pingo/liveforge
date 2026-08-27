package httpstream

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/ratelimit"
)

// Module implements the HTTP streaming module for FLV, TS, FMP4, HLS, and DASH.
type Module struct {
	server    *core.Server
	policyMu  sync.Mutex
	policy    config.HTTPConfig
	listener  net.Listener
	httpSrv   *http.Server
	limiter   *ratelimit.Limiter
	limiterMu sync.RWMutex
	rateCfg   config.RateLimitConfig
	wg        sync.WaitGroup

	// Track which stream instances have muxer callbacks registered.
	registeredMu sync.Mutex
	registered   map[*core.Stream]bool

	// HLS segment managers per stream key.
	hlsMu       sync.Mutex
	hlsManagers map[string]*HLSManager

	// DASH segment managers per stream key.
	dashMu       sync.Mutex
	dashManagers map[string]*DASHManager

	// LL-HLS segment managers per stream key.
	llhlsMu       sync.Mutex
	llhlsManagers map[string]*LLHLSManager
}

// NewModule creates a new HTTP streaming module.
func NewModule() *Module {
	return &Module{
		registered:    make(map[*core.Stream]bool),
		hlsManagers:   make(map[string]*HLSManager),
		dashManagers:  make(map[string]*DASHManager),
		llhlsManagers: make(map[string]*LLHLSManager),
	}
}

// Name returns the module name.
func (m *Module) Name() string { return "httpstream" }

// Init initializes the HTTP server.
func (m *Module) Init(s *core.Server) error {
	m.server = s
	cfg := s.Config()
	m.policyMu.Lock()
	m.policy = cfg.HTTP
	m.policyMu.Unlock()

	ln, err := s.MakeListenerAutoTLS(cfg.HTTP.Listen, cfg.HTTP.TLS)
	if err != nil {
		return err
	}
	m.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/{path...}", m.handleWebSocket)
	mux.HandleFunc("/{path...}", m.handleStream)
	handler := http.Handler(mux)
	if rl := cfg.Limits.RateLimit; rl.Enabled && rl.Rate > 0 {
		m.limiter = ratelimit.New(rl.Rate, rl.Burst)
	}
	m.rateCfg = cfg.Limits.RateLimit
	m.httpSrv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.limiterMu.RLock()
		limiter := m.limiter
		m.limiterMu.RUnlock()
		if limiter != nil && !limiter.AllowRequest(r) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		handler.ServeHTTP(w, r)
	})}

	proto := "http"
	if s.HasTLS() && (cfg.HTTP.TLS == nil || *cfg.HTTP.TLS) {
		proto = "https"
	}
	slog.Info("listening", "module", "httpstream", "proto", proto, "addr", ln.Addr())

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("serve error", "module", "httpstream", "error", err)
		}
	}()

	return nil
}

// Hooks returns the module's event hooks. We listen for publish stop and
// stream destroy events to clean up HLS/DASH managers.
func (m *Module) Hooks() []core.HookRegistration {
	return []core.HookRegistration{
		{
			Event:    core.EventPublishStop,
			Mode:     core.HookAsync,
			Priority: 100,
			Consumer: "httpstream",
			Handler:  m.onPublishStop,
		},
		{
			Event:    core.EventStreamDestroy,
			Mode:     core.HookAsync,
			Priority: 100,
			Consumer: "httpstream",
			Handler:  m.onStreamDestroy,
		},
	}
}

// OnReload stops segment managers that captured HLS/DASH/LL-HLS policy so
// subsequent requests recreate them from the new immutable server snapshot.
// Listener, TLS, CORS, and module enablement changes remain restart-required.
func (m *Module) OnReload(s *core.Server) error {
	m.updateRateLimiter(s.Config().Limits.RateLimit)
	next := s.Config().HTTP
	m.policyMu.Lock()
	previous := m.policy
	m.policy = next
	m.policyMu.Unlock()
	if reflect.DeepEqual(previous.HLS, next.HLS) && reflect.DeepEqual(previous.DASH, next.DASH) && reflect.DeepEqual(previous.LLHLS, next.LLHLS) {
		return nil
	}
	m.hlsMu.Lock()
	for key, manager := range m.hlsManagers {
		manager.Stop()
		delete(m.hlsManagers, key)
	}
	m.hlsMu.Unlock()
	m.dashMu.Lock()
	for key, manager := range m.dashManagers {
		manager.Stop()
		delete(m.dashManagers, key)
	}
	m.dashMu.Unlock()
	m.llhlsMu.Lock()
	for key, manager := range m.llhlsManagers {
		manager.Stop()
		delete(m.llhlsManagers, key)
	}
	m.llhlsMu.Unlock()
	return nil
}

func (m *Module) updateRateLimiter(cfg config.RateLimitConfig) {
	m.limiterMu.RLock()
	unchanged := m.rateCfg == cfg
	m.limiterMu.RUnlock()
	if unchanged {
		return
	}
	var next *ratelimit.Limiter
	if cfg.Enabled && cfg.Rate > 0 {
		next = ratelimit.New(cfg.Rate, cfg.Burst)
	}
	m.limiterMu.Lock()
	old := m.limiter
	m.limiter = next
	m.rateCfg = cfg
	m.limiterMu.Unlock()
	if old != nil {
		old.Close()
	}
}

func (m *Module) onPublishStop(ctx *core.EventContext) error {
	m.cleanupManagers(ctx.StreamKey)
	return nil
}

func (m *Module) onStreamDestroy(ctx *core.EventContext) error {
	m.cleanupManagers(ctx.StreamKey)
	return nil
}

// cleanupManagers stops and removes HLS/DASH/LL-HLS managers for a stream.
func (m *Module) cleanupManagers(streamKey string) {
	m.hlsMu.Lock()
	if mgr, ok := m.hlsManagers[streamKey]; ok {
		mgr.Stop()
		delete(m.hlsManagers, streamKey)
	}
	m.hlsMu.Unlock()

	m.dashMu.Lock()
	if mgr, ok := m.dashManagers[streamKey]; ok {
		mgr.Stop()
		delete(m.dashManagers, streamKey)
	}
	m.dashMu.Unlock()

	m.llhlsMu.Lock()
	if mgr, ok := m.llhlsManagers[streamKey]; ok {
		mgr.Stop()
		delete(m.llhlsManagers, streamKey)
	}
	m.llhlsMu.Unlock()
}

// getOrCreateHLS returns (or creates) an HLS manager for the given stream.
func (m *Module) getOrCreateHLS(streamKey string, stream *core.Stream) *HLSManager {
	m.hlsMu.Lock()
	defer m.hlsMu.Unlock()

	if mgr, ok := m.hlsManagers[streamKey]; ok {
		return mgr
	}

	cfg := m.server.Config().HTTP.HLS
	targetDur := cfg.SegmentDuration
	playlistSize := cfg.PlaylistSize

	// basePath is the URL prefix for segment references in the m3u8
	basePath := "/" + escapeStreamKeyPath(streamKey)
	mgr := NewHLSManager(streamKey, basePath, targetDur, playlistSize)
	m.hlsManagers[streamKey] = mgr
	go mgr.Run(stream)
	return mgr
}

// getOrCreateDASH returns (or creates) a DASH manager for the given stream.
func (m *Module) getOrCreateDASH(streamKey string, stream *core.Stream) *DASHManager {
	m.dashMu.Lock()
	defer m.dashMu.Unlock()

	if mgr, ok := m.dashManagers[streamKey]; ok {
		return mgr
	}

	cfg := m.server.Config().HTTP.DASH
	targetDur := cfg.SegmentDuration
	playlistSize := cfg.PlaylistSize

	basePath := "/" + escapeStreamKeyPath(streamKey)
	mgr := NewDASHManager(streamKey, basePath, targetDur, playlistSize)
	mgr.InitFromStream(stream) // compute init segment synchronously
	m.dashManagers[streamKey] = mgr
	go mgr.Run(stream)
	return mgr
}

// getOrCreateLLHLS returns (or creates) an LL-HLS manager for the given stream.
func (m *Module) getOrCreateLLHLS(streamKey string, stream *core.Stream) *LLHLSManager {
	m.llhlsMu.Lock()
	defer m.llhlsMu.Unlock()

	if mgr, ok := m.llhlsManagers[streamKey]; ok {
		return mgr
	}

	cfg := m.server.Config().HTTP.LLHLS
	basePath := "/" + escapeStreamKeyPath(streamKey)
	mgr := NewLLHLSManager(streamKey, basePath, cfg.PartDuration, cfg.SegmentDuration, cfg.SegmentCount, cfg.Container)
	m.llhlsManagers[streamKey] = mgr
	go mgr.Run(stream)
	return mgr
}

// Close shuts down the HTTP server and all managers.
func (m *Module) Close() error {
	if m.httpSrv != nil {
		// Shutdown gracefully cancels in-flight request contexts, unblocking
		// handlers that poll with r.Context().Done() (e.g., DASH segment holds).
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		m.httpSrv.Shutdown(ctx) //nolint:errcheck
		cancel()
	}

	m.limiterMu.Lock()
	limiter := m.limiter
	m.limiter = nil
	m.limiterMu.Unlock()
	if limiter != nil {
		limiter.Close()
	}

	// Stop all HLS managers
	m.hlsMu.Lock()
	for key, mgr := range m.hlsManagers {
		mgr.Stop()
		delete(m.hlsManagers, key)
	}
	m.hlsMu.Unlock()

	// Stop all DASH managers
	m.dashMu.Lock()
	for key, mgr := range m.dashManagers {
		mgr.Stop()
		delete(m.dashManagers, key)
	}
	m.dashMu.Unlock()

	// Stop all LL-HLS managers
	m.llhlsMu.Lock()
	for key, mgr := range m.llhlsManagers {
		mgr.Stop()
		delete(m.llhlsManagers, key)
	}
	m.llhlsMu.Unlock()

	m.wg.Wait()
	slog.Info("stopped", "module", "httpstream")
	return nil
}

// Addr returns the listener address (useful for tests).
func (m *Module) Addr() net.Addr {
	if m.listener != nil {
		return m.listener.Addr()
	}
	return nil
}
