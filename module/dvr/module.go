package dvr

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
)

// Module implements core.Module for DVR/time-shift playback.
type Module struct {
	server   *core.Server
	policy   atomic.Pointer[config.DVRConfig]
	listener net.Listener
	httpSrv  *http.Server
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	sessions map[string]*Session
	reloadCh chan struct{}
	metrics  DVRMetrics
}

// NewModule creates a new DVR module.
func NewModule() *Module {
	return &Module{
		sessions: make(map[string]*Session),
		reloadCh: make(chan struct{}, 1),
	}
}

// Name returns the module name.
func (m *Module) Name() string { return "dvr" }

// Init starts the DVR HTTP server and cleanup goroutine.
func (m *Module) Init(s *core.Server) error {
	m.server = s
	cfg := s.Config().DVR
	m.storePolicy(cfg)

	ln, err := s.MakeListenerAutoTLS(cfg.Listen, nil)
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
		"window", cfg.Window, "segment", cfg.SegmentDuration)

	return nil
}

// OnReload applies DVR retention and segmentation policy to new work. Active
// segments finish with their creation policy; cleanup observes the new window.
func (m *Module) OnReload(s *core.Server) error {
	cfg := s.Config().DVR
	current := m.Policy()
	cfg.Enabled = current.Enabled
	cfg.Listen = current.Listen
	m.storePolicy(cfg)
	select {
	case m.reloadCh <- struct{}{}:
	default:
	}
	return nil
}

func (m *Module) storePolicy(cfg config.DVRConfig) {
	owned := cfg
	m.policy.Store(&owned)
}

func (m *Module) Policy() config.DVRConfig {
	if cfg := m.policy.Load(); cfg != nil {
		return *cfg
	}
	return config.DVRConfig{}
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
		s.Wait()
	}

	m.wg.Wait()
	slog.Info("stopped", "module", "dvr")
	return nil
}

func (m *Module) onPublish(ctx *core.EventContext) error {
	cfg := m.Policy()
	if !matchPattern(cfg.StreamPattern, ctx.StreamKey) {
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

	session, err := newSession(ctx.StreamKey, stream, cfg, existingIndex, startSeq, &m.metrics)
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
		session.Wait()
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
		sessionState := session.Status()
		var totalDur float64
		for _, seg := range segs {
			totalDur += seg.Duration
		}
		result = append(result, DVRSessionStatus{
			StreamKey: key,
			Live:      session.IsLive(),
			Segments:  len(segs),
			Duration:  totalDur,
			Bytes:     segmentBytes(segs),
			StartedAt: sessionState.StartedAt,
			LastError: sessionState.LastError,
		})
		populateSegmentRange(&result[len(result)-1], segs)
	}
	m.mu.Unlock()
	sort.Slice(result, func(i, j int) bool { return result[i].StreamKey < result[j].StreamKey })
	return result
}

func (m *Module) DVRStatus() DVRStatusSnapshot {
	sessions, _ := m.SessionStatus().([]DVRSessionStatus)
	return DVRStatusSnapshot{Sessions: sessions, Storage: dvrStorageHealth(m.Policy().Path), Metrics: m.metrics.Snapshot()}
}

func (m *Module) DVRSession(streamKey string) (DVRSessionStatus, bool) {
	m.mu.Lock()
	session := m.sessions[streamKey]
	m.mu.Unlock()
	if session == nil {
		return DVRSessionStatus{}, false
	}
	segments := session.Index().Segments()
	var duration float64
	for _, segment := range segments {
		duration += segment.Duration
	}
	status := DVRSessionStatus{
		StreamKey: streamKey,
		Live:      session.IsLive(),
		Segments:  len(segments),
		Duration:  duration,
		Bytes:     segmentBytes(segments),
		StartedAt: session.Status().StartedAt,
		LastError: session.Status().LastError,
	}
	populateSegmentRange(&status, segments)
	return status, true
}

func populateSegmentRange(status *DVRSessionStatus, segments []Segment) {
	if len(segments) == 0 {
		return
	}
	status.OldestSegment = segments[0].StartTime
	last := segments[len(segments)-1]
	status.NewestSegment = last.StartTime.Add(time.Duration(last.Duration * float64(time.Second)))
}

func segmentBytes(segments []Segment) int64 {
	var total int64
	for _, segment := range segments {
		total += segment.Size
	}
	return total
}

// DVRSessionStatus represents a single DVR session's status.
type DVRSessionStatus struct {
	StreamKey     string    `json:"stream_key"`
	Live          bool      `json:"live"`
	Segments      int       `json:"segments"`
	Duration      float64   `json:"duration_sec"`
	Bytes         int64     `json:"bytes"`
	StartedAt     time.Time `json:"started_at"`
	OldestSegment time.Time `json:"oldest_segment,omitempty"`
	NewestSegment time.Time `json:"newest_segment,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
}

var _ core.Reloadable = (*Module)(nil)
