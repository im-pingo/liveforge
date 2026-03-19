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

	for {
		// Check if interleaved data ($ prefix) when in Playing/Recording state
		if session != nil && (session.State == StatePlaying || session.State == StateRecording) {
			b, err := reader.Peek(1)
			if err != nil {
				return
			}
			if b[0] == '$' {
				_, _, err := ReadInterleaved(reader)
				if err != nil {
					return
				}
				// TODO: route interleaved data to publisher.FeedRTP
				continue
			}
		}

		req, err := ReadRequest(reader)
		if err != nil {
			return
		}

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
			// Clean up session
			if session != nil {
				m.mu.Lock()
				delete(m.sessions, session.ID)
				m.mu.Unlock()
			}
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

// generateSessionID creates a random session ID.
func generateSessionID() string {
	buf := make([]byte, 8)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}
