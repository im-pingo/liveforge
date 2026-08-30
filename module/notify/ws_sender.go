package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const wsClientQueueDepth = 64

// WSSender manages WebSocket notification clients.
type WSSender struct {
	mu      sync.RWMutex
	clients map[*wsClient]struct{}
	dropped atomic.Uint64
}

type wsClient struct {
	conn      *websocket.Conn
	events    map[string]bool // nil means all events
	ctx       context.Context
	cancel    context.CancelFunc
	send      chan []byte
	done      chan struct{}
	closed    atomic.Bool
	closeOnce sync.Once
}

// NewWSSender creates a new WebSocket notification sender.
func NewWSSender() *WSSender {
	return &WSSender{
		clients: make(map[*wsClient]struct{}),
	}
}

// Send broadcasts a payload to all connected clients whose event filter matches.
func (s *WSSender) Send(p *NotifyPayload) {
	data, err := json.Marshal(p)
	if err != nil {
		slog.Error("ws marshal error", "module", "notify", "error", err)
		return
	}

	s.mu.RLock()
	clients := make([]*wsClient, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.mu.RUnlock()

	for _, c := range clients {
		if c.events != nil && !c.events[p.Event] {
			continue
		}
		if c.closed.Load() {
			continue
		}
		select {
		case c.send <- data:
		default:
			s.dropped.Add(1)
			slog.Warn("ws client queue full, dropping event", "module", "notify", "event", p.Event, "stream", p.StreamKey)
		}
	}
}

// HandleWebSocket upgrades an HTTP connection to a WebSocket notification stream.
// Query parameter "events" is a comma-separated list of event names to filter.
// If omitted, all events are sent.
func (s *WSSender) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	events := parseEventFilter(r.URL.Query().Get("events"))

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Error("ws accept error", "module", "notify", "error", err)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	c := &wsClient{
		conn:   conn,
		events: events,
		ctx:    ctx,
		cancel: cancel,
		send:   make(chan []byte, wsClientQueueDepth),
		done:   make(chan struct{}),
	}

	s.mu.Lock()
	s.clients[c] = struct{}{}
	s.mu.Unlock()

	slog.Info("ws client connected", "module", "notify", "events", events)
	go s.writeLoop(c)

	// Block until the client disconnects or context is cancelled.
	// Read loop detects close frames.
	for {
		_, _, err := c.conn.Read(ctx)
		if err != nil {
			break
		}
	}

	s.removeClient(c)
}

// ClientCount returns the number of connected WebSocket clients.
func (s *WSSender) ClientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.clients)
}

// Close disconnects all clients.
func (s *WSSender) Close() {
	s.mu.Lock()
	clients := make([]*wsClient, 0, len(s.clients))
	for c := range s.clients {
		clients = append(clients, c)
	}
	s.clients = make(map[*wsClient]struct{})
	s.mu.Unlock()

	for _, c := range clients {
		s.closeClient(c, websocket.StatusGoingAway, "server shutdown")
	}
}

func (s *WSSender) removeClient(c *wsClient) {
	s.mu.Lock()
	_, ok := s.clients[c]
	delete(s.clients, c)
	s.mu.Unlock()

	if ok {
		s.closeClient(c, websocket.StatusNormalClosure, "")
		slog.Info("ws client disconnected", "module", "notify")
	}
}

func (s *WSSender) writeLoop(c *wsClient) {
	for {
		select {
		case data := <-c.send:
			ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
			err := c.conn.Write(ctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				s.removeClient(c)
				return
			}
		case <-c.done:
			return
		}
	}
}

func (s *WSSender) closeClient(c *wsClient, status websocket.StatusCode, reason string) {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.done)
		c.cancel()
		_ = c.conn.Close(status, reason)
	})
}

// Dropped returns the number of notification messages rejected by full client
// queues since the sender was created.
func (s *WSSender) Dropped() uint64 {
	return s.dropped.Load()
}

// parseEventFilter parses a comma-separated event filter string.
// Returns nil if empty (meaning all events).
func parseEventFilter(raw string) map[string]bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	events := make(map[string]bool)
	for _, e := range strings.Split(raw, ",") {
		e = strings.TrimSpace(e)
		if e != "" {
			events[e] = true
		}
	}
	if len(events) == 0 {
		return nil
	}
	return events
}
