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
	server        *core.Server
	policyMu      sync.Mutex
	policy        config.HTTPConfig
	listener      net.Listener
	httpSrv       *http.Server
	limiter       *ratelimit.Limiter
	limiterMu     sync.RWMutex
	rateCfg       config.RateLimitConfig
	wg            sync.WaitGroup
	handlerMu     sync.Mutex
	handlerWG     sync.WaitGroup
	handlerCtx    context.Context
	handlerCancel context.CancelFunc
	closing       bool

	// Track the latest callback registration token per stream key. The registry
	// does not retain historical *core.Stream pointers.
	registeredMu sync.Mutex
	registered   map[string]uint64

	// HLS segment managers per stream key.
	hlsMu       sync.Mutex
	hlsManagers map[string]*HLSManager

	// DASH segment managers per stream key.
	dashMu       sync.Mutex
	dashManagers map[string]*DASHManager

	// LL-HLS segment managers per stream key.
	llhlsMu       sync.Mutex
	llhlsManagers map[string]*LLHLSManager

	// Retain only the latest retired identity per live stream key. Replacement
	// admission and stream destruction remove entries, bounding churn ownership.
	generationMu       sync.Mutex
	retiredGenerations map[string]managerGenerationIdentity

	managerMu       sync.Mutex
	managerWG       sync.WaitGroup
	managerClosing  bool
	runningManagers map[segmentManager]struct{}
}

type segmentManager interface {
	Stop()
}

type managerGenerationIdentity struct {
	streamInstanceID    uint64
	publisherGeneration uint64
	publisherID         string
}

func managerGenerationFromEvent(ctx *core.EventContext) (managerGenerationIdentity, bool) {
	if ctx == nil || ctx.StreamInstanceID == 0 || ctx.PublisherGeneration == 0 {
		return managerGenerationIdentity{}, false
	}
	return managerGenerationIdentity{
		streamInstanceID:    ctx.StreamInstanceID,
		publisherGeneration: ctx.PublisherGeneration,
		publisherID:         ctx.PublisherID,
	}, true
}

func managerGenerationFromSnapshot(snapshot core.StreamStartupSnapshot) managerGenerationIdentity {
	return managerGenerationIdentity{
		streamInstanceID:    snapshot.StreamInstanceID,
		publisherGeneration: snapshot.Generation,
		publisherID:         snapshot.PublisherID,
	}
}

func (i managerGenerationIdentity) samePosition(other managerGenerationIdentity) bool {
	return i.streamInstanceID == other.streamInstanceID && i.publisherGeneration == other.publisherGeneration
}

func (i managerGenerationIdentity) blocks(other managerGenerationIdentity) bool {
	// StartupSnapshot clears PublisherID after detach; instance plus generation
	// still identifies that retired publisher unambiguously.
	return i.samePosition(other) && (i.publisherID == other.publisherID || i.publisherID == "" || other.publisherID == "")
}

func (i managerGenerationIdentity) newerThan(other managerGenerationIdentity) bool {
	return i.streamInstanceID > other.streamInstanceID ||
		(i.streamInstanceID == other.streamInstanceID && i.publisherGeneration > other.publisherGeneration)
}

// NewModule creates a new HTTP streaming module.
func NewModule() *Module {
	return &Module{
		registered:         make(map[string]uint64),
		hlsManagers:        make(map[string]*HLSManager),
		dashManagers:       make(map[string]*DASHManager),
		llhlsManagers:      make(map[string]*LLHLSManager),
		retiredGenerations: make(map[string]managerGenerationIdentity),
		runningManagers:    make(map[segmentManager]struct{}),
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
	m.handlerCtx, m.handlerCancel = context.WithCancel(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/{path...}", m.handleWebSocket)
	mux.HandleFunc("/{path...}", m.handleStream)
	handler := http.Handler(mux)
	if rl := cfg.Limits.RateLimit; rl.Enabled && rl.Rate > 0 {
		m.limiter = ratelimit.NewWithTrustedProxies(rl.Rate, rl.Burst, rl.TrustedProxies)
	}
	m.rateCfg = cfg.Limits.RateLimit
	m.httpSrv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.beginHandler() {
			http.Error(w, "server shutting down", http.StatusServiceUnavailable)
			return
		}
		defer m.handlerWG.Done()
		requestCtx, cancelRequest := context.WithCancel(r.Context())
		stopModuleCancel := context.AfterFunc(m.handlerCtx, cancelRequest)
		defer func() {
			stopModuleCancel()
			cancelRequest()
		}()
		r = r.WithContext(requestCtx)
		m.limiterMu.RLock()
		limiter := m.limiter
		m.limiterMu.RUnlock()
		if limiter != nil && !limiter.AllowRequest(r) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		handler.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute}

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

func (m *Module) beginHandler() bool {
	m.handlerMu.Lock()
	defer m.handlerMu.Unlock()
	if m.closing {
		return false
	}
	m.handlerWG.Add(1)
	return true
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
	m.stopRunningManagers()
	return nil
}

func (m *Module) updateRateLimiter(cfg config.RateLimitConfig) {
	m.limiterMu.RLock()
	unchanged := reflect.DeepEqual(m.rateCfg, cfg)
	m.limiterMu.RUnlock()
	if unchanged {
		return
	}
	var next *ratelimit.Limiter
	if cfg.Enabled && cfg.Rate > 0 {
		next = ratelimit.NewWithTrustedProxies(cfg.Rate, cfg.Burst, cfg.TrustedProxies)
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
	m.retireManagers(ctx.StreamKey, ctx)
	return nil
}

func (m *Module) onStreamDestroy(ctx *core.EventContext) error {
	m.retireManagersForLifecycle(ctx.StreamKey, ctx, false)
	if ctx.StreamInstanceID != 0 {
		m.registeredMu.Lock()
		if token := m.registered[ctx.StreamKey]; token == ctx.StreamInstanceID {
			delete(m.registered, ctx.StreamKey)
		}
		m.registeredMu.Unlock()
	}
	return nil
}

// retireManagers removes generation-matched managers from lookup while their
// workers drain the immutable publisher-generation boundary. Publish-stop also
// records the exact retired identity under the registration lock, closing the
// hook-before-create race without retaining historical stream pointers.
func (m *Module) retireManagers(streamKey string, ctx *core.EventContext) {
	m.retireManagersForLifecycle(streamKey, ctx, true)
}

func (m *Module) retireManagersForLifecycle(streamKey string, ctx *core.EventContext, rememberGeneration bool) {
	m.generationMu.Lock()
	defer m.generationMu.Unlock()
	if rememberGeneration {
		m.rememberRetiredGeneration(streamKey, ctx)
	} else {
		m.forgetDestroyedGeneration(streamKey, ctx)
	}

	matches := func(instanceID, generation uint64, publisherID string) bool {
		if ctx == nil {
			return true
		}
		if ctx.StreamInstanceID != 0 && instanceID != ctx.StreamInstanceID {
			return false
		}
		if ctx.PublisherGeneration != 0 && generation != ctx.PublisherGeneration {
			return false
		}
		return ctx.PublisherID == "" || publisherID == "" || publisherID == ctx.PublisherID
	}
	m.hlsMu.Lock()
	if mgr, ok := m.hlsManagers[streamKey]; ok {
		if matches(mgr.streamInstanceID, mgr.publisherGeneration, mgr.publisherID) {
			delete(m.hlsManagers, streamKey)
		}
	}
	m.hlsMu.Unlock()

	m.dashMu.Lock()
	if mgr, ok := m.dashManagers[streamKey]; ok {
		if matches(mgr.streamInstanceID, mgr.publisherGeneration, mgr.publisherID) {
			delete(m.dashManagers, streamKey)
		}
	}
	m.dashMu.Unlock()

	m.llhlsMu.Lock()
	if mgr, ok := m.llhlsManagers[streamKey]; ok {
		if matches(mgr.streamInstanceID, mgr.publisherGeneration, mgr.publisherID) {
			delete(m.llhlsManagers, streamKey)
		}
	}
	m.llhlsMu.Unlock()
}

func (m *Module) rememberRetiredGeneration(streamKey string, ctx *core.EventContext) {
	identity, exact := managerGenerationFromEvent(ctx)
	if !exact {
		return
	}
	m.managerMu.Lock()
	closing := m.managerClosing
	m.managerMu.Unlock()
	if closing {
		return
	}
	if m.server != nil {
		stream, found := m.server.StreamHub().Find(streamKey)
		if !found || stream.InstanceID() != identity.streamInstanceID {
			return
		}
	}
	if existing, found := m.retiredGenerations[streamKey]; found {
		if existing.newerThan(identity) {
			return
		}
		if existing.samePosition(identity) && existing.publisherID != "" && existing.publisherID != identity.publisherID {
			return
		}
	}
	m.retiredGenerations[streamKey] = identity
}

func (m *Module) forgetDestroyedGeneration(streamKey string, ctx *core.EventContext) {
	retired, found := m.retiredGenerations[streamKey]
	if !found {
		return
	}
	if ctx == nil || ctx.StreamInstanceID == 0 || retired.streamInstanceID == ctx.StreamInstanceID {
		delete(m.retiredGenerations, streamKey)
	}
}

func (m *Module) generationWasRetired(streamKey string, identity managerGenerationIdentity) bool {
	retired, found := m.retiredGenerations[streamKey]
	if !found {
		return false
	}
	if retired.blocks(identity) {
		return true
	}
	if identity.newerThan(retired) {
		delete(m.retiredGenerations, streamKey)
	}
	return false
}

// getOrCreateHLS returns (or creates) an HLS manager for the given stream, or
// nil when the resolved publisher generation has already retired.
func (m *Module) getOrCreateHLS(streamKey string, stream *core.Stream) *HLSManager {
	m.generationMu.Lock()
	defer m.generationMu.Unlock()
	m.hlsMu.Lock()
	defer m.hlsMu.Unlock()
	snapshot := stream.StartupSnapshot()
	identity := managerGenerationFromSnapshot(snapshot)
	publisherID := snapshot.PublisherID
	if stream.State() != core.StreamStatePublishing || m.generationWasRetired(streamKey, identity) {
		return nil
	}

	if mgr, ok := m.hlsManagers[streamKey]; ok {
		if mgr.streamInstanceID == identity.streamInstanceID &&
			mgr.publisherGeneration == identity.publisherGeneration &&
			mgr.publisherID == identity.publisherID {
			return mgr
		}
		delete(m.hlsManagers, streamKey)
	}

	cfg := m.server.Config().HTTP.HLS
	targetDur := cfg.SegmentDuration
	playlistSize := cfg.PlaylistSize

	// basePath is the URL prefix for segment references in the m3u8
	basePath := "/" + escapeStreamKeyPath(streamKey)
	mgr := NewHLSManager(streamKey, basePath, targetDur, playlistSize)
	mgr.streamInstanceID = identity.streamInstanceID
	mgr.publisherGeneration = snapshot.Generation
	mgr.publisherID = publisherID
	if m.startManager(mgr, func() { mgr.Run(stream) }) {
		m.hlsManagers[streamKey] = mgr
	}
	return mgr
}

// getOrCreateDASH returns (or creates) a DASH manager for the given stream, or
// nil when the resolved publisher generation has already retired.
func (m *Module) getOrCreateDASH(streamKey string, stream *core.Stream) *DASHManager {
	m.generationMu.Lock()
	defer m.generationMu.Unlock()
	m.dashMu.Lock()
	defer m.dashMu.Unlock()
	snapshot := stream.StartupSnapshot()
	identity := managerGenerationFromSnapshot(snapshot)
	publisherID := snapshot.PublisherID
	if stream.State() != core.StreamStatePublishing || m.generationWasRetired(streamKey, identity) {
		return nil
	}

	if mgr, ok := m.dashManagers[streamKey]; ok {
		if mgr.streamInstanceID == identity.streamInstanceID &&
			mgr.publisherGeneration == identity.publisherGeneration &&
			mgr.publisherID == identity.publisherID {
			return mgr
		}
		delete(m.dashManagers, streamKey)
	}

	cfg := m.server.Config().HTTP.DASH
	targetDur := cfg.SegmentDuration
	playlistSize := cfg.PlaylistSize

	basePath := "/" + escapeStreamKeyPath(streamKey)
	mgr := NewDASHManager(streamKey, basePath, targetDur, playlistSize)
	mgr.streamInstanceID = identity.streamInstanceID
	mgr.publisherGeneration = snapshot.Generation
	mgr.publisherID = publisherID
	if m.startManager(mgr, func() { mgr.Run(stream) }) {
		m.dashManagers[streamKey] = mgr
	}
	return mgr
}

// getOrCreateLLHLS returns (or creates) an LL-HLS manager for the given stream,
// or nil when the resolved publisher generation has already retired.
func (m *Module) getOrCreateLLHLS(streamKey string, stream *core.Stream) *LLHLSManager {
	m.generationMu.Lock()
	defer m.generationMu.Unlock()
	m.llhlsMu.Lock()
	defer m.llhlsMu.Unlock()
	snapshot := stream.StartupSnapshot()
	identity := managerGenerationFromSnapshot(snapshot)
	publisherID := snapshot.PublisherID
	if stream.State() != core.StreamStatePublishing || m.generationWasRetired(streamKey, identity) {
		return nil
	}

	if mgr, ok := m.llhlsManagers[streamKey]; ok {
		if mgr.streamInstanceID == identity.streamInstanceID &&
			mgr.publisherGeneration == identity.publisherGeneration &&
			mgr.publisherID == identity.publisherID {
			return mgr
		}
		delete(m.llhlsManagers, streamKey)
	}

	cfg := m.server.Config().HTTP.LLHLS
	basePath := "/" + escapeStreamKeyPath(streamKey)
	mgr := NewLLHLSManager(streamKey, basePath, cfg.PartDuration, cfg.SegmentDuration, cfg.SegmentCount, cfg.Container)
	mgr.streamInstanceID = identity.streamInstanceID
	mgr.publisherGeneration = snapshot.Generation
	mgr.publisherID = publisherID
	if m.startManager(mgr, func() { mgr.Run(stream) }) {
		m.llhlsManagers[streamKey] = mgr
	}
	return mgr
}

func (m *Module) startManager(manager segmentManager, run func()) bool {
	m.managerMu.Lock()
	if m.managerClosing {
		m.managerMu.Unlock()
		manager.Stop()
		return false
	}
	m.runningManagers[manager] = struct{}{}
	m.managerWG.Add(1)
	m.managerMu.Unlock()

	go func() {
		defer func() {
			m.managerMu.Lock()
			delete(m.runningManagers, manager)
			m.managerMu.Unlock()
			m.managerWG.Done()
		}()
		run()
	}()
	return true
}

func (m *Module) takeRegisteredManagers() []segmentManager {
	var managers []segmentManager
	m.hlsMu.Lock()
	for key, manager := range m.hlsManagers {
		managers = append(managers, manager)
		delete(m.hlsManagers, key)
	}
	m.hlsMu.Unlock()
	m.dashMu.Lock()
	for key, manager := range m.dashManagers {
		managers = append(managers, manager)
		delete(m.dashManagers, key)
	}
	m.dashMu.Unlock()
	m.llhlsMu.Lock()
	for key, manager := range m.llhlsManagers {
		managers = append(managers, manager)
		delete(m.llhlsManagers, key)
	}
	m.llhlsMu.Unlock()
	return managers
}

func (m *Module) stopRunningManagers() {
	m.managerMu.Lock()
	managers := make([]segmentManager, 0, len(m.runningManagers))
	for manager := range m.runningManagers {
		managers = append(managers, manager)
	}
	m.managerMu.Unlock()
	managers = append(managers, m.takeRegisteredManagers()...)
	for _, manager := range managers {
		manager.Stop()
	}
}

func (m *Module) stopAndJoinManagers() {
	m.managerMu.Lock()
	m.managerClosing = true
	managers := make([]segmentManager, 0, len(m.runningManagers))
	for manager := range m.runningManagers {
		managers = append(managers, manager)
	}
	m.managerMu.Unlock()
	m.generationMu.Lock()
	clear(m.retiredGenerations)
	m.generationMu.Unlock()
	managers = append(managers, m.takeRegisteredManagers()...)
	for _, manager := range managers {
		manager.Stop()
	}
	m.managerWG.Wait()
}

// Close shuts down the HTTP server and all managers.
func (m *Module) Close() error {
	m.handlerMu.Lock()
	m.closing = true
	if m.handlerCancel != nil {
		m.handlerCancel()
	}
	m.handlerMu.Unlock()

	var shutdownErr error
	if m.httpSrv != nil {
		// Shutdown gracefully cancels in-flight request contexts, unblocking
		// handlers that poll with r.Context().Done() (e.g., DASH segment holds).
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		shutdownErr = m.httpSrv.Shutdown(ctx)
		cancel()
		if shutdownErr != nil {
			_ = m.httpSrv.Close()
		}
	}

	m.limiterMu.Lock()
	limiter := m.limiter
	m.limiter = nil
	m.limiterMu.Unlock()
	if limiter != nil {
		limiter.Close()
	}

	m.stopAndJoinManagers()

	m.handlerWG.Wait()
	m.wg.Wait()
	slog.Info("stopped", "module", "httpstream")
	return shutdownErr
}

// Addr returns the listener address (useful for tests).
func (m *Module) Addr() net.Addr {
	if m.listener != nil {
		return m.listener.Addr()
	}
	return nil
}
