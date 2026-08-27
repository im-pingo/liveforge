package rtsp

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/portalloc"
	pionrtp "github.com/pion/rtp/v2"
)

// Module implements core.Module for RTSP.
type Module struct {
	server    *core.Server
	listener  net.Listener
	handler   *Handler
	ports     *portalloc.PortAllocator
	sessions  map[string]*RTSPSession
	conns     map[net.Conn]struct{}
	mu        sync.Mutex
	done      chan struct{}
	wg        sync.WaitGroup
	closing   bool
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

var errRTSPDrainTimeout = errors.New("rtsp: drain timeout")

// NewModule creates a new RTSP module.
func NewModule() *Module {
	return &Module{
		sessions:  make(map[string]*RTSPSession),
		conns:     make(map[net.Conn]struct{}),
		done:      make(chan struct{}),
		closeDone: make(chan struct{}),
	}
}

func (m *Module) Name() string { return "rtsp" }

func (m *Module) Init(s *core.Server) error {
	m.server = s
	cfg := s.Config().RTSP

	if len(cfg.RTPPortRange) == 2 {
		m.ports, _ = portalloc.New(cfg.RTPPortRange[0], cfg.RTPPortRange[1])
	}
	if m.ports == nil {
		m.ports, _ = portalloc.New(30000, 40000) // default range
	}

	var mcast *config.MulticastConfig
	if cfg.Multicast.Enabled {
		InitMulticastPorts(cfg.Multicast.BasePort)
		mcast = &cfg.Multicast
	}

	m.handler = NewHandler(s, m.ports, mcast)

	ln, err := s.MakeListener(cfg.Listen, cfg.TLS)
	if err != nil {
		return fmt.Errorf("rtsp: listen %s: %w", cfg.Listen, err)
	}
	m.listener = ln

	m.wg.Add(2)
	go func() {
		defer m.wg.Done()
		m.acceptLoop()
	}()
	go func() {
		defer m.wg.Done()
		m.sessionReaper()
	}()

	proto := "rtsp"
	if s.Config().TLS.Configured() && (cfg.TLS == nil || *cfg.TLS) {
		proto = "rtsps"
	}
	slog.Info("listening", "module", "rtsp", "proto", proto, "addr", cfg.Listen)
	return nil
}

func (m *Module) Hooks() []core.HookRegistration { return nil }

func (m *Module) Close() error {
	m.closeOnce.Do(func() {
		m.closeErr = m.close()
		close(m.closeDone)
	})
	<-m.closeDone
	return m.closeErr
}

func (m *Module) close() error {
	// Set the admission gate before closing the listener. An Accept that won
	// the race with listener.Close observes closing under m.mu and is rejected.
	m.mu.Lock()
	m.closing = true
	listener := m.listener
	m.mu.Unlock()
	close(m.done)
	var listenerErr error
	if listener != nil {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			listenerErr = err
		}
	}

	m.mu.Lock()
	conns := make([]net.Conn, 0, len(m.conns))
	for conn := range m.conns {
		conns = append(conns, conn)
	}
	sessions := make([]*RTSPSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	for _, session := range sessions {
		m.cleanupSession(session)
	}

	waitDone := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(waitDone)
	}()
	timeout := 10 * time.Second
	if m.server != nil && m.server.Config().Server.DrainTimeout > 0 {
		timeout = m.server.Config().Server.DrainTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-waitDone:
		return listenerErr
	case <-timer.C:
		return errors.Join(listenerErr, errRTSPDrainTimeout)
	}
}

// sessionReaper periodically checks for expired sessions and cleans them up.
func (m *Module) sessionReaper() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			var expired []*RTSPSession
			for _, s := range m.sessions {
				if s.IsExpired() {
					expired = append(expired, s)
				}
			}
			m.mu.Unlock()
			for _, s := range expired {
				snapshot := s.Snapshot()
				slog.Debug("reaping expired session", "module", "rtsp", "session", snapshot.ID, "stream", snapshot.StreamKey)
				m.cleanupSession(s)
			}
		case <-m.done:
			return
		}
	}
}

func (m *Module) acceptLoop() {
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			select {
			case <-m.done:
				return
			default:
				slog.Error("accept error", "module", "rtsp", "error", err)
				continue
			}
		}
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			_ = conn.Close()
			return
		}
		if !m.server.AcquireConn() {
			m.mu.Unlock()
			slog.Warn("max connections reached", "module", "rtsp", "remote", conn.RemoteAddr())
			conn.Close()
			continue
		}
		m.conns[conn] = struct{}{}
		m.wg.Add(1)
		m.mu.Unlock()
		go m.handleConn(conn)
	}
}

func (m *Module) handleConn(conn net.Conn) {
	defer func() {
		_ = conn.Close()
		m.mu.Lock()
		delete(m.conns, conn)
		m.mu.Unlock()
		m.server.ReleaseConn()
		m.wg.Done()
	}()
	reader := bufio.NewReader(conn)
	var session *RTSPSession

	defer func() {
		if session != nil {
			m.cleanupSession(session)
		}
	}()

	for {
		if m.isClosing() {
			return
		}
		// Check if interleaved data ($ prefix) when in Recording state
		if session != nil && session.GetState() == StateRecording {
			b, err := reader.Peek(1)
			if err != nil {
				return
			}
			if b[0] == '$' {
				ch, data, err := ReadInterleaved(reader)
				if err != nil {
					return
				}
				if err := m.processInterleaved(session, ch, data); err != nil {
					return
				}
				continue
			}
		}

		req, err := ReadRequest(reader)
		if err != nil {
			return
		}
		if m.isClosing() {
			return
		}
		slog.Debug("request", "module", "rtsp", "method", req.Method, "url", req.URL)

		// Create or find session
		if session == nil && req.Method != "OPTIONS" {
			sessionID := generateSessionID()
			streamKey := extractStreamKey(req.URL)
			session = NewRTSPSession(sessionID, streamKey)
			session.SetRemoteAddr(conn.RemoteAddr().String())
			m.mu.Lock()
			if m.closing {
				m.mu.Unlock()
				session.Close()
				return
			}
			m.sessions[sessionID] = session
			m.mu.Unlock()
		}
		if session != nil {
			if !session.CanHandleRequest() {
				_ = WriteResponse(conn, newResponse(454, "Session Not Found", req))
				return
			}
			session.Touch()
		}

		var resp *Response
		switch req.Method {
		case "OPTIONS":
			resp = m.handler.HandleOptions(req)
		case "DESCRIBE":
			resp = m.handler.HandleDescribe(req, session)
		case "SETUP":
			resp = m.handler.HandleSetup(req, session, conn.RemoteAddr().String())
		case "PLAY":
			resp = m.handler.HandlePlay(req, session, conn.RemoteAddr().String())
			if resp.StatusCode == 200 {
				if err := WriteResponse(conn, resp); err != nil {
					return
				}
				m.runSubscriberLoop(conn, session)
				return
			}
		case "PAUSE":
			resp = m.handler.HandlePause(req, session)
		case "ANNOUNCE":
			resp = m.handler.HandleAnnounce(req, session, conn.RemoteAddr().String())
		case "RECORD":
			resp = m.handler.HandleRecord(req, session)
			snapshot := session.Snapshot()
			if resp.StatusCode == 200 && snapshot.Publisher != nil {
				// Start RTCP RR loop for TCP interleaved publisher.
				var rtcpCh uint8 = 1
				for _, t := range snapshot.Tracks {
					if t.Codec.IsVideo() && t.Transport.IsTCP {
						rtcpCh = uint8(t.Transport.Interleaved[1])
						break
					}
				}
				snapshot.Publisher.SetRTCPWriter(conn, rtcpCh)

				// Start UDP read loops for tracks using UDP transport.
				for _, t := range snapshot.Tracks {
					if t.UDP != nil {
						m.startWorker(func() { m.udpPublishLoop(t.UDP, session) })
					}
				}
			}
		case "TEARDOWN":
			resp = m.handler.HandleTeardown(req, session)
			if err := WriteResponse(conn, resp); err != nil {
				return
			}
			m.cleanupSession(session)
			return
		case "GET_PARAMETER":
			resp = m.handler.HandleGetParameter(req)
		default:
			resp = newResponse(405, "Method Not Allowed", req)
		}

		if err := WriteResponse(conn, resp); err != nil {
			return
		}
	}
}

func (m *Module) isClosing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closing
}

func (m *Module) connectionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.conns)
}

func (m *Module) startWorker(run func()) bool {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return false
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		run()
	}()
	return true
}

// runSubscriberLoop creates a subscriber and feeds frames from the stream to the RTSP client.
func (m *Module) runSubscriberLoop(conn net.Conn, session *RTSPSession) {
	snapshot := session.Snapshot()
	if snapshot.Closed || snapshot.Stream == nil || snapshot.MediaInfo == nil {
		return
	}

	// Determine video/audio channels and optional UDP/multicast transports from track setup.
	var videoChannel, audioChannel uint8
	var videoUDP, audioUDP *UDPTransport
	var videoMcast, audioMcast *MulticastTransport
	for _, t := range snapshot.Tracks {
		if t.Codec.IsVideo() {
			if t.Transport.IsTCP {
				videoChannel = uint8(t.Transport.Interleaved[0])
			} else if t.Multicast != nil {
				videoMcast = t.Multicast
			} else {
				videoUDP = t.UDP
			}
		}
		if t.Codec.IsAudio() {
			if t.Transport.IsTCP {
				audioChannel = uint8(t.Transport.Interleaved[0])
			} else if t.Multicast != nil {
				audioMcast = t.Multicast
			} else {
				audioUDP = t.UDP
			}
		}
	}

	sub, err := NewRTSPSubscriber(snapshot.ID, snapshot.MediaInfo, conn, videoChannel, audioChannel)
	if err != nil {
		slog.Error("failed to create subscriber", "module", "rtsp", "session", snapshot.ID, "error", err)
		return
	}
	sub.videoUDP = videoUDP
	sub.audioUDP = audioUDP
	sub.videoMulticast = videoMcast
	sub.audioMulticast = audioMcast
	if err := snapshot.Stream.AddSubscriber("rtsp"); err != nil {
		slog.Warn("subscriber limit reached", "module", "rtsp", "session", snapshot.ID, "error", err)
		sub.Close()
		return
	}
	if !session.SetSubscriber(sub) {
		snapshot.Stream.RemoveSubscriber("rtsp")
		sub.Close()
		return
	}
	if !session.startSubscribeLifecycle(func() {
		m.server.GetEventBus().EmitAsync(core.EventSubscribe, &core.EventContext{
			StreamKey:    snapshot.StreamKey,
			SubscriberID: snapshot.ID,
			Protocol:     "rtsp",
			RemoteAddr:   snapshot.RemoteAddr,
		})
	}) {
		session.ClearSubscriber(sub)
		snapshot.Stream.RemoveSubscriber("rtsp")
		sub.Close()
		return
	}

	defer func() {
		sub.Close()
		snapshot.Stream.RemoveSubscriber("rtsp")
		session.ClearSubscriber(sub)
	}()

	// Note: SPS/PPS are delivered via SDP sprop-parameter-sets.
	// Sending VideoSeqHeader as a separate RTP frame causes duplicate
	// timestamps with the first keyframe. Skip it for RTSP.

	// Send GOP cache for instant playback (atomic snapshot with cursor).
	// Skip SequenceHeader frames — SPS/PPS is delivered via SDP sprop-parameter-sets.
	gopCache, startPos := snapshot.Stream.GOPCacheSnapshot()
	for _, frame := range gopCache {
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if err := sub.SendFrame(frame); err != nil {
			return
		}
	}

	// Start reading right after the snapshot position to avoid duplicating GOP frames.
	ringReader := snapshot.Stream.RingBuffer().NewReaderAt(startPos)
	filter := core.NewSlowConsumerFilter(ringReader, snapshot.Stream.Config().SlowConsumer, m.server.Config().RTSP.SkipTracker)
	for {
		frame, ok := filter.NextFrame()
		if !ok {
			return
		}
		select {
		case <-sub.Done():
			return
		default:
		}
		// Skip SequenceHeader — delivered via SDP.
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		start := time.Now()
		if err := sub.SendFrame(frame); err != nil {
			return
		}
		filter.ReportSendTime(time.Since(start))
	}
}

// udpPublishLoop reads RTP packets from a UDP transport and feeds them to the publisher.
func (m *Module) udpPublishLoop(ut *UDPTransport, session *RTSPSession) {
	buf := make([]byte, 2048)
	for {
		select {
		case <-ut.done:
			return
		default:
		}
		n, _, err := ut.ReadRTP(buf)
		if err != nil {
			return
		}
		publisher, err := activePublisherForIngress(session)
		if err != nil {
			return
		}
		pkt := &pionrtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err == nil {
			if err := publisher.FeedRTP(pkt); err != nil {
				return
			}
		}
		session.Touch()
	}
}

func (m *Module) processInterleaved(session *RTSPSession, channel uint8, data []byte) error {
	if session == nil {
		return nil
	}
	publisher, err := activePublisherForIngress(session)
	if err != nil {
		return err
	}
	if channel%2 != 0 {
		session.Touch()
		return nil
	}
	pkt := &pionrtp.Packet{}
	if err := pkt.Unmarshal(data); err == nil {
		if err := publisher.FeedRTP(pkt); err != nil {
			return err
		}
	}
	session.Touch()
	return nil
}

func activePublisherForIngress(session *RTSPSession) (*RTSPPublisher, error) {
	snapshot := session.Snapshot()
	publisher := snapshot.Publisher
	if snapshot.Closed || publisher == nil || snapshot.Stream == nil || publisher.stream != snapshot.Stream {
		return nil, fmt.Errorf("RTSP session %s has no active publisher", snapshot.ID)
	}
	publisher.mu.Lock()
	closed := publisher.closed
	publisher.mu.Unlock()
	if closed || snapshot.Stream.Publisher() != publisher {
		return nil, fmt.Errorf("RTSP publisher %s no longer owns stream", publisher.id)
	}
	return publisher, nil
}

func (m *Module) cleanupSession(session *RTSPSession) bool {
	if session == nil {
		return false
	}
	snapshot, closed := session.closeSnapshot()
	if !closed {
		return false
	}
	m.mu.Lock()
	if m.sessions[snapshot.ID] == session {
		delete(m.sessions, snapshot.ID)
	}
	m.mu.Unlock()
	if snapshot.Published && snapshot.Publisher != nil {
		m.server.GetEventBus().EmitAsync(core.EventPublishStop, &core.EventContext{
			StreamKey:   snapshot.StreamKey,
			PublisherID: snapshot.Publisher.ID(),
			Protocol:    "rtsp",
			RemoteAddr:  snapshot.RemoteAddr,
		})
	}
	if snapshot.Subscribed {
		m.server.GetEventBus().EmitAsync(core.EventSubscribeStop, &core.EventContext{
			StreamKey:    snapshot.StreamKey,
			SubscriberID: snapshot.ID,
			Protocol:     "rtsp",
			RemoteAddr:   snapshot.RemoteAddr,
		})
	}
	return true
}

// interleavedChannelToMediaType maps an interleaved channel to a media type
// based on the session's track setup.
func interleavedChannelToMediaType(session *RTSPSession, channel uint8) avframe.MediaType {
	for _, t := range session.Snapshot().Tracks {
		if t.Transport.IsTCP && uint8(t.Transport.Interleaved[0]) == channel {
			if t.Codec.IsVideo() {
				return avframe.MediaTypeVideo
			}
			return avframe.MediaTypeAudio
		}
	}
	return 0
}

// generateSessionID creates a random session ID.
func generateSessionID() string {
	buf := make([]byte, 8)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}
