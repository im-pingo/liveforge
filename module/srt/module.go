package srt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	slog.Info("listening", "module", "srt", "addr", cfg.SRT.Listen, "latency_ms", cfg.SRT.Latency)

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
	slog.Info("stopped", "module", "srt")
	return nil
}

func (m *Module) serveLoop() {
	defer m.wg.Done()
	if err := m.srtSrv.Serve(); err != nil && err != gosrt.ErrServerClosed {
		slog.Error("serve error", "module", "srt", "error", err)
	}
}

// parseStreamID parses the SRT stream ID into mode and stream key.
// Supported formats:
//   - "publish:/live/stream1"  → publish, "live/stream1"
//   - "subscribe:/live/stream1" → subscribe, "live/stream1"
//   - "#!::r=/live/stream1,m=publish" → publish, "live/stream1"
//   - "/live/stream1" → subscribe (default), "live/stream1"
func parseStreamID(streamID string) (mode string, streamKey string) {
	parsed := parseStreamRequest(streamID)
	return parsed.Mode, parsed.StreamKey
}

type streamRequest struct {
	Mode      string
	StreamKey string
	Params    map[string]string
}

// parseStreamRequest keeps authorization parameters separate from the
// canonical stream key for both URL and SRT access-control stream IDs.
func parseStreamRequest(streamID string) streamRequest {
	// Format: "mode:path" (e.g., "publish:/live/stream1")
	if strings.HasPrefix(streamID, "publish:") {
		key, params := parseStreamResource(strings.TrimPrefix(streamID, "publish:"))
		return streamRequest{Mode: "publish", StreamKey: key, Params: params}
	}
	if strings.HasPrefix(streamID, "subscribe:") {
		key, params := parseStreamResource(strings.TrimPrefix(streamID, "subscribe:"))
		return streamRequest{Mode: "subscribe", StreamKey: key, Params: params}
	}

	// Format: "#!::key=value,key=value" (SRT Access Control)
	if strings.HasPrefix(streamID, "#!::") {
		params := strings.TrimPrefix(streamID, "#!::")
		var resource, modeVal string
		authParams := make(map[string]string)
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
			default:
				authParams[parts[0]] = parts[1]
			}
		}
		if modeVal == "" {
			modeVal = "subscribe"
		}
		key, resourceParams := parseStreamResource(resource)
		for name, value := range resourceParams {
			authParams[name] = value
		}
		if len(authParams) == 0 {
			authParams = nil
		}
		return streamRequest{Mode: modeVal, StreamKey: key, Params: authParams}
	}

	// Default: treat as subscribe with the streamID as path
	key, params := parseStreamResource(streamID)
	return streamRequest{Mode: "subscribe", StreamKey: key, Params: params}
}

func parseStreamResource(raw string) (string, map[string]string) {
	u, err := url.Parse(raw)
	if err != nil {
		return strings.TrimPrefix(raw, "/"), nil
	}
	params := make(map[string]string)
	for name, values := range u.Query() {
		if len(values) > 0 {
			params[name] = values[0]
		}
	}
	if len(params) == 0 {
		params = nil
	}
	return strings.TrimPrefix(u.Path, "/"), params
}

// normalizeStreamKey strips leading slash and URL query parameters.
func normalizeStreamKey(raw string) string {
	// Parse as URL to strip query string
	if u, err := url.Parse(raw); err == nil {
		raw = u.Path
	}
	return strings.TrimPrefix(raw, "/")
}

var errEncryptionRequired = errors.New("SRT encryption is required")

func validateSRTEncryption(passphrase string, encrypted bool) error {
	if passphrase != "" && !encrypted {
		return errEncryptionRequired
	}
	return nil
}

// handleConnect is the SRT server callback for incoming connections.
func (m *Module) handleConnect(req gosrt.ConnRequest) gosrt.ConnType {
	if !m.server.AcquireConn() {
		slog.Warn("max connections reached", "module", "srt", "remote", req.RemoteAddr())
		return gosrt.REJECT
	}

	streamID := req.StreamId()
	parsed := parseStreamRequest(streamID)
	mode, streamKey := parsed.Mode, parsed.StreamKey

	if streamKey == "" {
		slog.Warn("empty stream key", "module", "srt", "remote", req.RemoteAddr())
		m.server.ReleaseConn()
		return gosrt.REJECT
	}

	cfg := m.server.RuntimeConfig().SRT()
	if err := validateSRTEncryption(cfg.Passphrase, req.IsEncrypted()); err != nil {
		slog.Warn("unencrypted connection rejected", "module", "srt", "remote", req.RemoteAddr())
		m.server.ReleaseConn()
		return gosrt.REJECT
	}
	if cfg.Passphrase != "" {
		if err := req.SetPassphrase(cfg.Passphrase); err != nil {
			slog.Warn("passphrase mismatch", "module", "srt", "remote", req.RemoteAddr(), "error", err)
			m.server.ReleaseConn()
			return gosrt.REJECT
		}
	}

	switch mode {
	case "publish":
		if err := m.authorizeSRT(core.AuthorizationPublish, core.AuthorizationPreSession, parsed, req.RemoteAddr().String()); err != nil {
			slog.Warn("publish auth rejected", "module", "srt", "stream", streamKey, "error", err)
			m.server.ReleaseConn()
			return gosrt.REJECT
		}
		return gosrt.PUBLISH

	case "subscribe", "request":
		if err := m.authorizeSRT(core.AuthorizationSubscribe, core.AuthorizationPreSession, parsed, req.RemoteAddr().String()); err != nil {
			slog.Warn("subscribe auth rejected", "module", "srt", "stream", streamKey, "error", err)
			m.server.ReleaseConn()
			return gosrt.REJECT
		}
		return gosrt.SUBSCRIBE

	default:
		slog.Warn("unknown mode", "module", "srt", "mode", mode, "remote", req.RemoteAddr())
		m.server.ReleaseConn()
		return gosrt.REJECT
	}
}

// handlePublish is the SRT server callback for publishing connections.
func (m *Module) handlePublish(conn gosrt.Conn) {
	defer m.server.ReleaseConn()
	defer conn.Close()

	parsed := parseStreamRequest(conn.StreamId())
	streamKey := parsed.StreamKey
	if streamKey == "" {
		return
	}
	if err := m.authorizeSRT(core.AuthorizationPublish, core.AuthorizationPostConnect, parsed, conn.RemoteAddr().String()); err != nil {
		slog.Warn("post-connect publish auth rejected", "module", "srt", "stream", streamKey, "error", err)
		return
	}

	pub := NewPublisher(conn, streamKey, m.hub, m.eventBus)

	m.pubsMu.Lock()
	if _, exists := m.pubs[streamKey]; exists {
		m.pubsMu.Unlock()
		slog.Warn("stream already publishing", "module", "srt", "stream", streamKey)
		return
	}
	m.pubs[streamKey] = pub
	m.pubsMu.Unlock()

	defer func() {
		m.pubsMu.Lock()
		delete(m.pubs, streamKey)
		m.pubsMu.Unlock()
	}()

	slog.Info("publish start", "module", "srt", "stream", streamKey, "remote", conn.RemoteAddr())
	pub.Run()
	slog.Info("publish stop", "module", "srt", "stream", streamKey)
}

// handleSubscribe is the SRT server callback for subscribing connections.
func (m *Module) handleSubscribe(conn gosrt.Conn) {
	defer m.server.ReleaseConn()
	defer conn.Close()

	parsed := parseStreamRequest(conn.StreamId())
	streamKey := parsed.StreamKey
	if streamKey == "" {
		return
	}
	if err := m.authorizeSRT(core.AuthorizationSubscribe, core.AuthorizationPostConnect, parsed, conn.RemoteAddr().String()); err != nil {
		slog.Warn("post-connect subscribe auth rejected", "module", "srt", "stream", streamKey, "error", err)
		return
	}

	sub := NewSubscriber(conn, streamKey, m.hub, m.eventBus, m.server.RuntimeConfig().SRT().SkipTracker)

	slog.Info("subscribe start", "module", "srt", "stream", streamKey, "remote", conn.RemoteAddr())
	sub.Run()
	slog.Info("subscribe stop", "module", "srt", "stream", streamKey)
}

func (m *Module) authorizeSRT(action core.AuthorizationAction, stage core.AuthorizationStage, parsed streamRequest, remoteAddr string) error {
	return m.server.Authorize(context.Background(), core.AuthorizationRequest{
		Action: action, Stage: stage, StreamKey: parsed.StreamKey,
		Protocol: "srt", RemoteAddr: remoteAddr, Params: parsed.Params,
	})
}
