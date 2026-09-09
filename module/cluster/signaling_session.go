package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/portalloc"
)

const maxSignalingRequestBytes int64 = 64 << 10

// relaySessions owns in-flight setup as well as accepted media sessions. Close
// prevents new admission, cancels socket/reader waits, and joins their cleanup.
type relaySessions struct {
	mu     sync.Mutex
	closed bool
	active map[*relaySession]struct{}
	wg     sync.WaitGroup
}

type relaySession struct {
	ctx         context.Context
	cancel      context.CancelFunc
	owner       *relaySessions
	watchers    sync.WaitGroup
	cleanup     []func()
	once        sync.Once
	established bool
}

func (g *relaySessions) begin(parent context.Context) (*relaySession, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, fmt.Errorf("relay transport is closed")
	}
	ctx, cancel := context.WithCancel(parent)
	s := &relaySession{ctx: ctx, cancel: cancel, owner: g}
	if g.active == nil {
		g.active = make(map[*relaySession]struct{})
	}
	g.active[s] = struct{}{}
	g.wg.Add(1)
	return s, nil
}

func (g *relaySessions) close() error {
	g.mu.Lock()
	g.closed = true
	for s := range g.active {
		s.cancel()
	}
	g.mu.Unlock()
	g.wg.Wait()
	return nil
}

func (s *relaySession) finish() {
	s.once.Do(func() {
		s.cancel()
		s.watchers.Wait()
		for i := len(s.cleanup) - 1; i >= 0; i-- {
			s.cleanup[i]()
		}
		s.owner.mu.Lock()
		delete(s.owner.active, s)
		s.owner.mu.Unlock()
		s.owner.wg.Done()
	})
}

func (s *relaySession) ownSocket(conn *net.UDPConn) {
	s.watchers.Add(1)
	go func() { defer s.watchers.Done(); <-s.ctx.Done(); _ = conn.Close() }()
}

func (s *relaySession) bindGeneration(snapshot core.StreamStartupSnapshot) {
	s.watchers.Add(1)
	go func() {
		defer s.watchers.Done()
		select {
		case <-snapshot.GenerationDone:
			s.cancel()
		case <-s.ctx.Done():
		}
	}()
}

func (s *relaySession) listenUDP(ports *portalloc.PortAllocator) (*net.UDPConn, int, error) {
	port, err := ports.Allocate()
	if err != nil {
		return nil, 0, err
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
	if err != nil {
		ports.Free(port)
		return nil, 0, err
	}
	s.ownSocket(conn)
	s.cleanup = append(s.cleanup, func() { ports.Free(port) })
	return conn, port, nil
}

func (s *relaySession) listenUDPPair(ports *portalloc.PortAllocator) (*net.UDPConn, int, error) {
	pair, err := ports.AllocateBoundUDPPair("udp", nil)
	if err != nil {
		return nil, 0, err
	}
	s.ownSocket(pair.RTPConn)
	s.ownSocket(pair.RTCPConn)
	s.cleanup = append(s.cleanup, func() { ports.Free(pair.RTPPort, pair.RTCPPort) })
	return pair.RTPConn, pair.RTPPort, nil
}

func (s *relaySession) dialUDP(ports *portalloc.PortAllocator, remote *net.UDPAddr) (*net.UDPConn, error) {
	port, err := ports.Allocate()
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", &net.UDPAddr{Port: port}, remote)
	if err != nil {
		ports.Free(port)
		return nil, err
	}
	s.ownSocket(conn)
	s.cleanup = append(s.cleanup, func() { ports.Free(port) })
	return conn, nil
}

func readSignalingRequest(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSignalingRequestBytes))
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, http.StatusText(status), status)
		return nil, false
	}
	return body, true
}

func admitSignaledPublisher(session *relaySession, server *core.Server, hub *core.StreamHub, r *http.Request, kind, protocol string, info *avframe.MediaInfo) (*core.Stream, *originPublisher, int, error) {
	key := r.URL.Query().Get("stream")
	pub := newOriginPublisher(kind, key, info)
	event := &core.EventContext{StreamKey: key, PublisherID: pub.ID(), Protocol: protocol, RemoteAddr: r.RemoteAddr, Params: make(map[string]string)}
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			event.Params[key] = values[0]
		}
	}
	if server != nil {
		if err := server.GetEventBus().EmitSync(core.EventPublish, event); err != nil {
			return nil, nil, http.StatusForbidden, err
		}
	}
	stream, created, err := hub.GetOrCreateWithCreated(key)
	if err != nil {
		return nil, nil, http.StatusServiceUnavailable, err
	}
	if created {
		session.cleanup = append(session.cleanup, func() {
			if !session.established {
				stream.DiscardFailedAdmission(pub.ID())
			}
		})
	}
	if status, err := admitRelayPublisher(session, server, stream, pub, event); err != nil {
		return nil, nil, status, err
	}
	return stream, pub, 0, nil
}

func admitRelayPublisher(session *relaySession, server *core.Server, stream *core.Stream, pub *originPublisher, event *core.EventContext) (int, error) {
	if err := stream.SetPublisher(pub); err != nil {
		return http.StatusConflict, err
	}
	started := false
	session.cleanup = append(session.cleanup, func() {
		stream.RemovePublisherIf(pub)
		if started && server != nil {
			_ = server.GetEventBus().EmitAsync(core.EventPublishStop, event)
		}
	})
	snapshot := stream.StartupSnapshot()
	if snapshot.PublisherID != pub.ID() || !stream.IsPublisherGeneration(snapshot.Generation) {
		return http.StatusServiceUnavailable, fmt.Errorf("publisher retired during admission")
	}
	session.bindGeneration(snapshot)
	event.StreamInstanceID, event.PublisherGeneration = snapshot.StreamInstanceID, snapshot.Generation
	if session.ctx.Err() != nil {
		return http.StatusServiceUnavailable, session.ctx.Err()
	}
	if server != nil {
		if err := server.GetEventBus().EmitAsync(core.EventPublish, event); err != nil {
			return http.StatusServiceUnavailable, err
		}
	}
	started = true
	return 0, nil
}

func (s *relaySession) subscribe(stream *core.Stream, snapshot core.StreamStartupSnapshot, protocol string) error {
	release, err := stream.AddSubscriberForGeneration(protocol, snapshot.Generation)
	if err != nil {
		return err
	}
	s.cleanup = append(s.cleanup, release)
	return nil
}
