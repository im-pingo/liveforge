package rtsp

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

// Module implements core.Module for RTSP.
type Module struct {
	server      *core.Server
	listener    net.Listener
	handler     *Handler
	portManager *PortManager
	sessions    map[string]*RTSPSession
	mu          sync.Mutex
	done        chan struct{}
}

// NewModule creates a new RTSP module.
func NewModule() *Module {
	return &Module{
		sessions: make(map[string]*RTSPSession),
		done:     make(chan struct{}),
	}
}

func (m *Module) Name() string { return "rtsp" }

func (m *Module) Init(s *core.Server) error {
	m.server = s
	cfg := s.Config().RTSP

	if len(cfg.RTPPortRange) == 2 {
		m.portManager = NewPortManager(cfg.RTPPortRange[0], cfg.RTPPortRange[1])
	} else {
		m.portManager = NewPortManager(30000, 40000) // default range
	}

	m.handler = NewHandler(s, m.portManager)

	ln, err := s.MakeListener(cfg.Listen, cfg.TLS)
	if err != nil {
		return fmt.Errorf("rtsp: listen %s: %w", cfg.Listen, err)
	}
	m.listener = ln

	go m.acceptLoop()
	go m.sessionReaper()

	proto := "rtsp"
	if s.Config().TLS.Configured() && (cfg.TLS == nil || *cfg.TLS) {
		proto = "rtsps"
	}
	slog.Info("listening", "module", "rtsp", "proto", proto, "addr", cfg.Listen)
	return nil
}

func (m *Module) Hooks() []core.HookRegistration { return nil }

func (m *Module) Close() error {
	close(m.done)
	if m.listener != nil {
		return m.listener.Close()
	}
	return nil
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
			for id, s := range m.sessions {
				if s.IsExpired() {
					expired = append(expired, s)
					delete(m.sessions, id)
				}
			}
			m.mu.Unlock()
			for _, s := range expired {
				slog.Debug("reaping expired session", "module", "rtsp", "session", s.ID, "stream", s.StreamKey)
				s.Close()
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
		if !m.server.AcquireConn() {
			slog.Warn("max connections reached", "module", "rtsp", "remote", conn.RemoteAddr())
			conn.Close()
			continue
		}
		go m.handleConn(conn)
	}
}

func (m *Module) handleConn(conn net.Conn) {
	defer conn.Close()
	defer m.server.ReleaseConn()
	reader := bufio.NewReader(conn)
	var session *RTSPSession

	defer func() {
		if session != nil {
			// Emit stop events before cleanup.
			if session.Publisher != nil {
				m.server.GetEventBus().Emit(core.EventPublishStop, &core.EventContext{
					StreamKey:  session.StreamKey,
					Protocol:   "rtsp",
					RemoteAddr: conn.RemoteAddr().String(),
				})
			}
			if session.Subscriber != nil {
				m.server.GetEventBus().Emit(core.EventSubscribeStop, &core.EventContext{
					StreamKey:  session.StreamKey,
					Protocol:   "rtsp",
					RemoteAddr: conn.RemoteAddr().String(),
				})
			}
			session.Close()
			m.mu.Lock()
			delete(m.sessions, session.ID)
			m.mu.Unlock()
		}
	}()

	for {
		// Check if interleaved data ($ prefix) when in Recording state
		if session != nil && session.State == StateRecording {
			b, err := reader.Peek(1)
			if err != nil {
				return
			}
			if b[0] == '$' {
				ch, data, err := ReadInterleaved(reader)
				if err != nil {
					return
				}
				// Odd channels carry RTCP — skip for now.
				if ch%2 != 0 {
					continue
				}
				if session.Publisher != nil {
					pkt := &pionrtp.Packet{}
					if err := pkt.Unmarshal(data); err == nil {
						session.Publisher.FeedRTP(pkt)
					}
				}
				continue
			}
		}

		req, err := ReadRequest(reader)
		if err != nil {
			return
		}
		slog.Debug("request", "module", "rtsp", "method", req.Method, "url", req.URL)

		// Create or find session
		if session == nil && req.Method != "OPTIONS" {
			sessionID := generateSessionID()
			streamKey := extractStreamKey(req.URL)
			session = NewRTSPSession(sessionID, streamKey)
			m.mu.Lock()
			m.sessions[sessionID] = session
			m.mu.Unlock()
		}
		if session != nil {
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
			if resp.StatusCode == 200 && session != nil && session.Publisher != nil {
				// Start RTCP RR loop for TCP interleaved publisher.
				var rtcpCh uint8 = 1
				for _, t := range session.Tracks {
					if t.Codec.IsVideo() && t.Transport.IsTCP {
						rtcpCh = uint8(t.Transport.Interleaved[1])
						break
					}
				}
				session.Publisher.SetRTCPWriter(conn, rtcpCh)

				// Start UDP read loops for tracks using UDP transport.
				for _, t := range session.Tracks {
					if t.UDP != nil {
						go m.udpPublishLoop(t.UDP, session)
					}
				}
			}
		case "TEARDOWN":
			resp = m.handler.HandleTeardown(req, session)
			if err := WriteResponse(conn, resp); err != nil {
				return
			}
			session.Close()
			m.mu.Lock()
			delete(m.sessions, session.ID)
			m.mu.Unlock()
			session = nil
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

// runSubscriberLoop creates a subscriber and feeds frames from the stream to the RTSP client.
func (m *Module) runSubscriberLoop(conn net.Conn, session *RTSPSession) {
	if session.Stream == nil || session.MediaInfo == nil {
		return
	}

	// Determine video/audio channels and optional UDP transports from track setup.
	var videoChannel, audioChannel uint8
	var videoUDP, audioUDP *UDPTransport
	for _, t := range session.Tracks {
		if t.Codec.IsVideo() {
			if t.Transport.IsTCP {
				videoChannel = uint8(t.Transport.Interleaved[0])
			} else {
				videoUDP = t.UDP
			}
		}
		if t.Codec.IsAudio() {
			if t.Transport.IsTCP {
				audioChannel = uint8(t.Transport.Interleaved[0])
			} else {
				audioUDP = t.UDP
			}
		}
	}

	sub, err := NewRTSPSubscriber(session.ID, session.MediaInfo, conn, videoChannel, audioChannel)
	if err != nil {
		slog.Error("failed to create subscriber", "module", "rtsp", "session", session.ID, "error", err)
		return
	}
	sub.videoUDP = videoUDP
	sub.audioUDP = audioUDP
	session.Subscriber = sub
	if err := session.Stream.AddSubscriber("rtsp"); err != nil {
		slog.Warn("subscriber limit reached", "module", "rtsp", "session", session.ID, "error", err)
		sub.Close()
		return
	}

	defer func() {
		sub.Close()
		session.Stream.RemoveSubscriber("rtsp")
		session.Subscriber = nil
	}()

	// Note: SPS/PPS are delivered via SDP sprop-parameter-sets.
	// Sending VideoSeqHeader as a separate RTP frame causes duplicate
	// timestamps with the first keyframe. Skip it for RTSP.

	// Send GOP cache for instant playback.
	// Skip SequenceHeader frames — SPS/PPS is delivered via SDP sprop-parameter-sets.
	for _, frame := range session.Stream.GOPCache() {
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}
		if err := sub.SendFrame(frame); err != nil {
			return
		}
	}

	// Start reading from the current write position to avoid duplicating GOP frames.
	ringReader := session.Stream.RingBuffer().NewReaderAt(session.Stream.RingBuffer().WriteCursor())
	filter := core.NewSlowConsumerFilter(ringReader, session.Stream.Config().SlowConsumer, m.server.Config().RTSP.SkipTracker)
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
		if session.Publisher != nil {
			pkt := &pionrtp.Packet{}
			if err := pkt.Unmarshal(buf[:n]); err == nil {
				session.Publisher.FeedRTP(pkt)
			}
		}
	}
}

// interleavedChannelToMediaType maps an interleaved channel to a media type
// based on the session's track setup.
func interleavedChannelToMediaType(session *RTSPSession, channel uint8) avframe.MediaType {
	for _, t := range session.Tracks {
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
