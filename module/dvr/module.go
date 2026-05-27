package dvr

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

// Module implements core.Module for DVR/time-shift playback.
type Module struct {
	server   *core.Server
	cfg      config.DVRConfig
	listener net.Listener
	httpSrv  *http.Server
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	sessions map[string]*Session
}

// NewModule creates a new DVR module.
func NewModule() *Module {
	return &Module{
		sessions: make(map[string]*Session),
	}
}

// Name returns the module name.
func (m *Module) Name() string { return "dvr" }

// Init starts the DVR HTTP server and cleanup goroutine.
func (m *Module) Init(s *core.Server) error {
	m.server = s
	m.cfg = s.Config().DVR

	ln, err := s.MakeListenerAutoTLS(m.cfg.Listen, nil)
	if err != nil {
		return err
	}
	m.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("GET /dvr/{app}/{key}.m3u8", m.handlePlaylist)
	mux.HandleFunc("GET /dvr/{app}/{key}/{filename}", m.handleSegment)

	m.httpSrv = &http.Server{Handler: mux}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	m.wg.Add(2)
	go func() {
		defer m.wg.Done()
		if err := m.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("serve error", "module", "dvr", "error", err)
		}
	}()
	go func() {
		defer m.wg.Done()
		m.runCleanup(ctx)
	}()

	slog.Info("listening", "module", "dvr", "addr", ln.Addr(),
		"window", m.cfg.Window, "segment", m.cfg.SegmentDuration)

	return nil
}

// Hooks returns async hooks for publish start/stop events.
func (m *Module) Hooks() []core.HookRegistration {
	return []core.HookRegistration{
		{
			Event:    core.EventPublish,
			Mode:     core.HookAsync,
			Priority: 60,
			Handler:  m.onPublish,
		},
		{
			Event:    core.EventPublishStop,
			Mode:     core.HookAsync,
			Priority: 60,
			Handler:  m.onPublishStop,
		},
	}
}

// Close shuts down the DVR server and stops all sessions.
func (m *Module) Close() error {
	if m.cancel != nil {
		m.cancel()
	}
	if m.httpSrv != nil {
		m.httpSrv.Close()
	}

	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	for _, s := range sessions {
		s.Stop()
	}

	m.wg.Wait()
	slog.Info("stopped", "module", "dvr")
	return nil
}

func (m *Module) onPublish(ctx *core.EventContext) error {
	if !matchPattern(m.cfg.StreamPattern, ctx.StreamKey) {
		return nil
	}

	stream, ok := m.server.StreamHub().Find(ctx.StreamKey)
	if !ok {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	existing := m.sessions[ctx.StreamKey]
	if existing != nil && existing.IsLive() {
		return nil
	}

	var existingIndex *SegmentIndex
	startSeq := 0
	if existing != nil {
		existingIndex = existing.Index()
		if last, ok := existingIndex.Last(); ok {
			startSeq = last.SeqNum + 1
		}
	}

	session, err := NewSession(ctx.StreamKey, stream, m.cfg, existingIndex, startSeq)
	if err != nil {
		slog.Error("failed to start dvr session", "module", "dvr", "stream", ctx.StreamKey, "error", err)
		return nil
	}

	m.sessions[ctx.StreamKey] = session
	go session.Run()
	slog.Info("started", "module", "dvr", "stream", ctx.StreamKey)
	return nil
}

func (m *Module) onPublishStop(ctx *core.EventContext) error {
	m.mu.Lock()
	session := m.sessions[ctx.StreamKey]
	m.mu.Unlock()

	if session != nil {
		session.Stop()
		slog.Info("stream stopped, segments retained", "module", "dvr", "stream", ctx.StreamKey)
	}
	return nil
}

// SessionStatus returns the status of all DVR sessions (implements DVRStatusProvider for the API module).
func (m *Module) SessionStatus() any {
	m.mu.Lock()
	result := make([]DVRSessionStatus, 0, len(m.sessions))
	for key, session := range m.sessions {
		segs := session.Index().Segments()
		var totalDur float64
		for _, seg := range segs {
			totalDur += seg.Duration
		}
		result = append(result, DVRSessionStatus{
			StreamKey: key,
			Live:      session.IsLive(),
			Segments:  len(segs),
			Duration:  totalDur,
		})
	}
	m.mu.Unlock()
	return result
}

// DVRSessionStatus represents a single DVR session's status.
type DVRSessionStatus struct {
	StreamKey string  `json:"stream_key"`
	Live      bool    `json:"live"`
	Segments  int     `json:"segments"`
	Duration  float64 `json:"duration_sec"`
}
