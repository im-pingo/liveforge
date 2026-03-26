package httpstream

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
)

// Module implements the HTTP streaming module for FLV, TS, FMP4, HLS, and DASH.
type Module struct {
	server   *core.Server
	listener net.Listener
	httpSrv  *http.Server
	wg       sync.WaitGroup

	// Track which stream instances have muxer callbacks registered.
	registeredMu sync.Mutex
	registered   map[*core.Stream]bool

	// HLS segment managers per stream key.
	hlsMu       sync.Mutex
	hlsManagers map[string]*HLSManager

	// DASH segment managers per stream key.
	dashMu       sync.Mutex
	dashManagers map[string]*DASHManager
}

// NewModule creates a new HTTP streaming module.
func NewModule() *Module {
	return &Module{
		registered:   make(map[*core.Stream]bool),
		hlsManagers:  make(map[string]*HLSManager),
		dashManagers: make(map[string]*DASHManager),
	}
}

// Name returns the module name.
func (m *Module) Name() string { return "httpstream" }

// Init initializes the HTTP server.
func (m *Module) Init(s *core.Server) error {
	m.server = s
	cfg := s.Config()

	ln, err := net.Listen("tcp", cfg.HTTP.Listen)
	if err != nil {
		return err
	}
	m.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/{path...}", m.handleWebSocket)
	mux.HandleFunc("/{path...}", m.handleStream)
	m.httpSrv = &http.Server{Handler: mux}

	log.Printf("[httpstream] listening on %s", ln.Addr())

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[httpstream] serve error: %v", err)
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
			Handler:  m.onPublishStop,
		},
		{
			Event:    core.EventStreamDestroy,
			Mode:     core.HookAsync,
			Priority: 100,
			Handler:  m.onStreamDestroy,
		},
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

// cleanupManagers stops and removes HLS/DASH managers for a stream.
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
	basePath := "/" + streamKey
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

	basePath := "/" + streamKey
	mgr := NewDASHManager(streamKey, basePath, targetDur, playlistSize)
	mgr.InitFromStream(stream) // compute init segment synchronously
	m.dashManagers[streamKey] = mgr
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

	m.wg.Wait()
	log.Println("[httpstream] stopped")
	return nil
}

// Addr returns the listener address (useful for tests).
func (m *Module) Addr() net.Addr {
	if m.listener != nil {
		return m.listener.Addr()
	}
	return nil
}
