package rtsp

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"sync"

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

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("rtsp: listen %s: %w", cfg.Listen, err)
	}
	m.listener = ln

	go m.acceptLoop()

	log.Printf("rtsp: listening on %s", cfg.Listen)
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

func (m *Module) acceptLoop() {
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			select {
			case <-m.done:
				return
			default:
				log.Printf("rtsp: accept error: %v", err)
				continue
			}
		}
		go m.handleConn(conn)
	}
}

func (m *Module) handleConn(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	var session *RTSPSession

	defer func() {
		if session != nil {
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
				// Skip RTCP channels (odd numbers are RTCP per RFC 2326)
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
		log.Printf("rtsp: %s %s", req.Method, req.URL)

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
			resp = m.handler.HandleSetup(req, session)
		case "PLAY":
			resp = m.handler.HandlePlay(req, session)
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
			resp = m.handler.HandleAnnounce(req, session)
		case "RECORD":
			resp = m.handler.HandleRecord(req, session)
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

	// Determine video/audio channels from track setup.
	var videoChannel, audioChannel uint8
	for _, t := range session.Tracks {
		if t.Codec.IsVideo() && t.Transport.IsTCP {
			videoChannel = uint8(t.Transport.Interleaved[0])
		}
		if t.Codec.IsAudio() && t.Transport.IsTCP {
			audioChannel = uint8(t.Transport.Interleaved[0])
		}
	}

	sub, err := NewRTSPSubscriber(session.ID, session.MediaInfo, conn, videoChannel, audioChannel)
	if err != nil {
		log.Printf("rtsp: failed to create subscriber for session %s: %v", session.ID, err)
		return
	}
	session.Subscriber = sub
	session.Stream.AddSubscriber("rtsp")

	defer func() {
		sub.Close()
		session.Stream.RemoveSubscriber("rtsp")
		session.Subscriber = nil
	}()

	// Send video sequence header (SPS/PPS) for decoder initialization.
	if seqHeader := session.Stream.VideoSeqHeader(); seqHeader != nil {
		if err := sub.SendFrame(seqHeader); err != nil {
			return
		}
	}

	// Send GOP cache for instant playback.
	for _, frame := range session.Stream.GOPCache() {
		if err := sub.SendFrame(frame); err != nil {
			return
		}
	}

	// Read from ring buffer and send frames.
	ringReader := session.Stream.RingBuffer().NewReader()
	for {
		frame, ok := ringReader.Read()
		if !ok {
			return
		}
		select {
		case <-sub.Done():
			return
		default:
		}
		if err := sub.SendFrame(frame); err != nil {
			return
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
