package srt

import (
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	gosrt "github.com/datarhei/gosrt"
	"github.com/im-pingo/liveforge/core"
)

// Module implements core.Module for the SRT protocol.
type Module struct {
	server   *core.Server
	hub      *core.StreamHub
	eventBus *core.EventBus
	srtSrv   *gosrt.Server
	wg       sync.WaitGroup
	closing  chan struct{}

	pubsMu sync.Mutex
	pubs   map[string]*Publisher // streamKey -> publisher
}

// NewModule creates a new SRT module.
func NewModule() *Module {
	return &Module{
		closing: make(chan struct{}),
		pubs:    make(map[string]*Publisher),
	}
}

// Name returns the module name.
func (m *Module) Name() string { return "srt" }

// Init initializes the SRT module and starts the listener.
func (m *Module) Init(s *core.Server) error {
	m.server = s
	m.eventBus = s.GetEventBus()
	m.hub = s.StreamHub()

	cfg := s.Config()

	srtConfig := gosrt.DefaultConfig()
	srtConfig.ReceiverLatency = time.Duration(cfg.SRT.Latency) * time.Millisecond
	srtConfig.PeerLatency = time.Duration(cfg.SRT.Latency) * time.Millisecond

	if cfg.SRT.Passphrase != "" {
		srtConfig.Passphrase = cfg.SRT.Passphrase
		if cfg.SRT.PBKeyLen > 0 {
			srtConfig.PBKeylen = cfg.SRT.PBKeyLen
		}
	}

	m.srtSrv = &gosrt.Server{
		Addr:            cfg.SRT.Listen,
		Config:          &srtConfig,
		HandleConnect:   m.handleConnect,
		HandlePublish:   m.handlePublish,
		HandleSubscribe: m.handleSubscribe,
	}

	if err := m.srtSrv.Listen(); err != nil {
		return fmt.Errorf("SRT listen on %s: %w", cfg.SRT.Listen, err)
	}

	log.Printf("SRT listening on %s (latency %dms)", cfg.SRT.Listen, cfg.SRT.Latency)

	m.wg.Add(1)
	go m.serveLoop()

	return nil
}

// Hooks returns hook registrations (none for SRT).
func (m *Module) Hooks() []core.HookRegistration { return nil }

// Close stops the SRT module.
func (m *Module) Close() error {
	close(m.closing)
	m.srtSrv.Shutdown()
	m.wg.Wait()
	log.Println("SRT module stopped")
	return nil
}

func (m *Module) serveLoop() {
	defer m.wg.Done()
	if err := m.srtSrv.Serve(); err != nil && err != gosrt.ErrServerClosed {
		log.Printf("SRT serve error: %v", err)
	}
}

// parseStreamID parses the SRT stream ID into mode and stream key.
// Supported formats:
//   - "publish:/live/stream1"  → publish, "live/stream1"
//   - "subscribe:/live/stream1" → subscribe, "live/stream1"
//   - "#!::r=/live/stream1,m=publish" → publish, "live/stream1"
//   - "/live/stream1" → subscribe (default), "live/stream1"
func parseStreamID(streamID string) (mode string, streamKey string) {
	// Format: "mode:path" (e.g., "publish:/live/stream1")
	if strings.HasPrefix(streamID, "publish:") {
		return "publish", normalizeStreamKey(strings.TrimPrefix(streamID, "publish:"))
	}
	if strings.HasPrefix(streamID, "subscribe:") {
		return "subscribe", normalizeStreamKey(strings.TrimPrefix(streamID, "subscribe:"))
	}

	// Format: "#!::key=value,key=value" (SRT Access Control)
	if strings.HasPrefix(streamID, "#!::") {
		params := strings.TrimPrefix(streamID, "#!::")
		var resource, modeVal string
		for _, kv := range strings.Split(params, ",") {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				continue
			}
			switch parts[0] {
			case "r":
				resource = parts[1]
			case "m":
				modeVal = parts[1]
			}
		}
		if modeVal == "" {
			modeVal = "subscribe"
		}
		return modeVal, normalizeStreamKey(resource)
	}

	// Default: treat as subscribe with the streamID as path
	return "subscribe", normalizeStreamKey(streamID)
}

// normalizeStreamKey strips leading slash and URL query parameters.
func normalizeStreamKey(raw string) string {
	// Parse as URL to strip query string
	if u, err := url.Parse(raw); err == nil {
		raw = u.Path
	}
	return strings.TrimPrefix(raw, "/")
}

// handleConnect is the SRT server callback for incoming connections.
func (m *Module) handleConnect(req gosrt.ConnRequest) gosrt.ConnType {
	if !m.server.AcquireConn() {
		log.Printf("SRT: max connections reached, rejecting %s", req.RemoteAddr())
		return gosrt.REJECT
	}

	streamID := req.StreamId()
	mode, streamKey := parseStreamID(streamID)

	if streamKey == "" {
		log.Printf("SRT: empty stream key from %s", req.RemoteAddr())
		m.server.ReleaseConn()
		return gosrt.REJECT
	}

	cfg := m.server.Config()
	if req.IsEncrypted() && cfg.SRT.Passphrase != "" {
		if err := req.SetPassphrase(cfg.SRT.Passphrase); err != nil {
			log.Printf("SRT: passphrase mismatch from %s: %v", req.RemoteAddr(), err)
			m.server.ReleaseConn()
			return gosrt.REJECT
		}
	}

	switch mode {
	case "publish":
		// Fire EventPublish via event bus for auth check
		ctx := &core.EventContext{
			StreamKey:  streamKey,
			Protocol:   "srt",
			RemoteAddr: req.RemoteAddr().String(),
		}
		if err := m.eventBus.Emit(core.EventPublish, ctx); err != nil {
			log.Printf("SRT: publish auth rejected for %s: %v", streamKey, err)
			m.server.ReleaseConn()
			return gosrt.REJECT
		}
		return gosrt.PUBLISH

	case "subscribe", "request":
		ctx := &core.EventContext{
			StreamKey:  streamKey,
			Protocol:   "srt",
			RemoteAddr: req.RemoteAddr().String(),
		}
		if err := m.eventBus.Emit(core.EventSubscribe, ctx); err != nil {
			log.Printf("SRT: subscribe auth rejected for %s: %v", streamKey, err)
			m.server.ReleaseConn()
			return gosrt.REJECT
		}
		return gosrt.SUBSCRIBE

	default:
		log.Printf("SRT: unknown mode %q from %s", mode, req.RemoteAddr())
		m.server.ReleaseConn()
		return gosrt.REJECT
	}
}

// handlePublish is the SRT server callback for publishing connections.
func (m *Module) handlePublish(conn gosrt.Conn) {
	defer m.server.ReleaseConn()
	defer conn.Close()

	_, streamKey := parseStreamID(conn.StreamId())
	if streamKey == "" {
		return
	}

	pub := NewPublisher(conn, streamKey, m.hub, m.eventBus)

	m.pubsMu.Lock()
	if _, exists := m.pubs[streamKey]; exists {
		m.pubsMu.Unlock()
		log.Printf("SRT: stream %s already publishing", streamKey)
		return
	}
	m.pubs[streamKey] = pub
	m.pubsMu.Unlock()

	defer func() {
		m.pubsMu.Lock()
		delete(m.pubs, streamKey)
		m.pubsMu.Unlock()
	}()

	log.Printf("SRT publish start: %s from %s", streamKey, conn.RemoteAddr())
	pub.Run()
	log.Printf("SRT publish stop: %s", streamKey)
}

// handleSubscribe is the SRT server callback for subscribing connections.
func (m *Module) handleSubscribe(conn gosrt.Conn) {
	defer m.server.ReleaseConn()
	defer conn.Close()

	_, streamKey := parseStreamID(conn.StreamId())
	if streamKey == "" {
		return
	}

	sub := NewSubscriber(conn, streamKey, m.hub, m.eventBus)

	log.Printf("SRT subscribe start: %s from %s", streamKey, conn.RemoteAddr())
	sub.Run()
	log.Printf("SRT subscribe stop: %s", streamKey)
}
