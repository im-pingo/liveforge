package cluster

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	"github.com/im-pingo/liveforge/pkg/portalloc"
	pionrtp "github.com/pion/rtp/v2"
)

const maxRelayPSBytes = 4 << 20

func appendRelayPS(buf, fragment []byte) ([]byte, error) {
	if len(fragment) > maxRelayPSBytes-len(buf) {
		return nil, fmt.Errorf("PS assembly exceeds %d bytes", maxRelayPSBytes)
	}
	if size := len(buf) + len(fragment); size > cap(buf) {
		grown := make([]byte, len(buf), min(maxRelayPSBytes, max(size, cap(buf)*2)))
		copy(grown, buf)
		buf = grown
	}
	return append(buf, fragment...), nil
}

// GBTransport implements RelayTransport for GB28181 PS-over-RTP relay.
// Uses SDP-over-HTTP signaling similar to RTPTransport, but encapsulates
// frames in MPEG-PS format within RTP packets.
type GBTransport struct {
	cfg      config.ClusterGBConfig
	ports    *portalloc.PortAllocator
	hub      *core.StreamHub
	server   *core.Server
	sessions relaySessions
}

// NewGBTransport creates a new GB28181 relay transport.
func NewGBTransport(cfg config.ClusterGBConfig, s *core.Server) *GBTransport {
	minPort, maxPort := 30000, 30100
	if len(cfg.PortRange) == 2 {
		minPort, maxPort = cfg.PortRange[0], cfg.PortRange[1]
	}
	if cfg.RTCPInterval == 0 {
		cfg.RTCPInterval = 5 * time.Second
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.SignalingPath == "" {
		cfg.SignalingPath = "/api/relay/gb"
	}

	ports, err := portalloc.New(minPort, maxPort)
	if err != nil {
		slog.Warn("gb transport port allocator failed, using defaults", "module", "cluster", "error", err)
		ports, _ = portalloc.New(30000, 30100)
	}

	t := &GBTransport{
		cfg:    cfg,
		ports:  ports,
		hub:    s.StreamHub(),
		server: s,
	}

	// Register signaling handlers.
	pushPath := cfg.SignalingPath + "/push"
	pullPath := cfg.SignalingPath + "/pull"
	s.RegisterAPIHandler("POST "+pushPath, core.WithAPIPermission("server:mutate", http.HandlerFunc(t.handlePushSignal)))
	s.RegisterAPIHandler("POST "+pullPath, core.WithAPIPermission("server:mutate", http.HandlerFunc(t.handlePullSignal)))

	slog.Info("gb transport ready", "module", "cluster",
		"push_path", pushPath, "pull_path", pullPath)

	return t
}

func (t *GBTransport) Scheme() string { return "gb28181" }

func (t *GBTransport) Push(ctx context.Context, targetURL string, stream *core.Stream) error {
	u, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("parse target URL: %w", err)
	}
	owner, err := t.sessions.begin(ctx)
	if err != nil {
		return err
	}
	defer owner.finish()
	snapshot := stream.StartupSnapshot()
	if !stream.IsPublisherGeneration(snapshot.Generation) {
		return nil
	}
	owner.bindGeneration(snapshot)
	relayCtx := owner.ctx

	// Allocate local port pair
	rtpPort, _, err := t.ports.AllocatePair()
	if err != nil {
		return fmt.Errorf("allocate port pair: %w", err)
	}
	defer t.ports.Free(rtpPort, rtpPort+1)

	// Signal remote to start receiving
	sigURL := fmt.Sprintf("http://%s%s/push?stream=%s&port=%d",
		u.Host, t.cfg.SignalingPath, url.QueryEscape(strings.TrimPrefix(u.Path, "/")), rtpPort)

	body, err := t.postSignal(relayCtx, sigURL)
	if err != nil {
		return fmt.Errorf("signaling request: %w", err)
	}

	// Read remote port from response
	remotePort, _ := strconv.Atoi(string(bytes.TrimSpace(body)))
	if remotePort < 1 || remotePort > 65535 {
		return fmt.Errorf("invalid remote port from signaling")
	}

	// Open UDP connection to remote
	remoteAddr := &net.UDPAddr{
		IP:   net.ParseIP(u.Hostname()),
		Port: remotePort,
	}
	conn, err := net.DialUDP("udp", &net.UDPAddr{Port: rtpPort}, remoteAddr)
	if err != nil {
		return fmt.Errorf("dial UDP: %w", err)
	}
	defer conn.Close()
	owner.ownSocket(conn)

	slog.Info("gb relay push connected", "module", "cluster", "target", targetURL, "remote_port", remotePort)
	markRelayConnected(relayCtx)
	if !stream.IsPublisherGeneration(snapshot.Generation) {
		return nil
	}

	muxer := ps.NewMuxer()
	var seq uint16
	var ts uint32
	ssrc := uint32(rtpPort) // simple SSRC

	// Send sequence headers first
	if vsh := snapshot.VideoSequenceHeader; vsh != nil {
		if err := t.sendPSFrameObserved(relayCtx, conn, muxer, vsh, &seq, &ts, ssrc); err != nil {
			return fmt.Errorf("send video sequence header: %w", err)
		}
	}
	for _, frame := range snapshot.ReplayFrames {
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return nil
		}
		if err := t.sendPSFrameObserved(relayCtx, conn, muxer, frame, &seq, &ts, ssrc); err != nil {
			return fmt.Errorf("send replay frame: %w", err)
		}
	}

	reader := stream.RingBuffer().NewReaderAt(snapshot.LiveCursor)
	for {
		frame, ok, readErr := core.ReadFrameContext(relayCtx, reader)
		if readErr != nil {
			return fmt.Errorf("source ring continuity lost: %w", readErr)
		}
		if !ok {
			return nil
		}
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return nil
		}

		if err := t.sendPSFrameObserved(relayCtx, conn, muxer, frame, &seq, &ts, ssrc); err != nil {
			return fmt.Errorf("send frame: %w", err)
		}
	}
}

func (t *GBTransport) sendPSFrame(conn *net.UDPConn, muxer *ps.Muxer, frame *avframe.AVFrame, seq *uint16, ts *uint32, ssrc uint32) error {
	return t.sendPSFrameObserved(context.Background(), conn, muxer, frame, seq, ts, ssrc)
}

func (t *GBTransport) sendPSFrameObserved(ctx context.Context, conn *net.UDPConn, muxer *ps.Muxer, frame *avframe.AVFrame, seq *uint16, ts *uint32, ssrc uint32) error {
	psData, err := muxer.Pack(frame)
	if err != nil {
		return err
	}

	// Fragment PS data into RTP packets (max 1400 bytes payload)
	const maxPayload = 1400
	for off := 0; off < len(psData); off += maxPayload {
		end := off + maxPayload
		if end > len(psData) {
			end = len(psData)
		}

		pkt := &pionrtp.Packet{
			Header: pionrtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: *seq,
				Timestamp:      *ts,
				SSRC:           ssrc,
				Marker:         end == len(psData), // marker on last fragment
			},
			Payload: psData[off:end],
		}
		*seq++

		data, err := pkt.Marshal()
		if err != nil {
			return fmt.Errorf("marshal RTP: %w", err)
		}
		n, err := conn.Write(data)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("write RTP: %w", err)
		}
		recordRelayBytes(ctx, int64(n))
	}

	// Advance timestamp (90kHz clock, 40ms per frame at 25fps)
	*ts += 3600

	return nil
}

func (t *GBTransport) Pull(ctx context.Context, sourceURL string, stream *core.Stream) error {
	u, err := url.Parse(sourceURL)
	if err != nil {
		return fmt.Errorf("parse source URL: %w", err)
	}

	owner, err := t.sessions.begin(ctx)
	if err != nil {
		return err
	}
	defer owner.finish()
	ctx = owner.ctx

	// Bind before the peer begins sending media.
	conn, rtpPort, err := owner.listenUDPPair(t.ports)
	if err != nil {
		return fmt.Errorf("listen UDP pair: %w", err)
	}

	// Signal remote to start sending
	sigURL := fmt.Sprintf("http://%s%s/pull?stream=%s&port=%d",
		u.Host, t.cfg.SignalingPath, url.QueryEscape(strings.TrimPrefix(u.Path, "/")), rtpPort)

	if _, err := t.postSignal(ctx, sigURL); err != nil {
		return fmt.Errorf("signaling request: %w", err)
	}

	slog.Info("gb relay pull started", "module", "cluster", "source", sourceURL, "local_port", rtpPort)

	markRelayConnected(ctx)

	pub := newOriginPublisher("gb-pull", stream.Key(), &avframe.MediaInfo{})
	event := &core.EventContext{StreamKey: stream.Key(), PublisherID: pub.ID(), Protocol: "gb28181", RemoteAddr: u.Host}
	if _, err := admitRelayPublisher(owner, t.server, stream, pub, event); err != nil {
		return fmt.Errorf("set publisher: %w", err)
	}

	demuxer := ps.NewDemuxer()
	var psBuf []byte

	buf := make([]byte, 2048)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		conn.SetReadDeadline(time.Now().Add(t.cfg.Timeout))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				return fmt.Errorf("receive timeout")
			}
			return fmt.Errorf("read UDP: %w", err)
		}

		if n < 12 {
			continue
		}
		recordRelayBytes(ctx, int64(n))

		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		psBuf, err = appendRelayPS(psBuf, pkt.Payload)
		if err != nil {
			return err
		}

		if pkt.Marker {
			frames, err := demuxer.Feed(psBuf)
			if err != nil {
				slog.Debug("ps demux error in gb pull", "module", "cluster", "error", err)
			}
			for _, frame := range frames {
				if frame.MediaType.IsAudio() {
					// The PS demuxer borrows audio payload storage from psBuf.
					frame.Payload = bytes.Clone(frame.Payload)
				}
				if !stream.WriteFrameForPublisher(pub, frame) && stream.Publisher() != pub {
					return nil
				}
			}
			psBuf = psBuf[:0]
		}
	}
}

func (t *GBTransport) postSignal(ctx context.Context, sigURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sigURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/plain")
	if err := authorizePeerRequest(req, t.server); err != nil {
		return nil, fmt.Errorf("authorize peer signaling: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		discardPeerSignalingResponse(resp.Body)
		return nil, peerSignalingStatusError(resp.StatusCode)
	}
	return readPeerSignalingResponse(resp.Body)
}

func (t *GBTransport) Close() error { return t.sessions.close() }

// handlePushSignal handles the signaling request for an incoming GB push relay.
func (t *GBTransport) handlePushSignal(w http.ResponseWriter, r *http.Request) {
	if _, ok := readSignalingRequest(w, r); !ok {
		return
	}
	streamKey := r.URL.Query().Get("stream")
	remotePortStr := r.URL.Query().Get("port")
	remotePort, err := strconv.Atoi(remotePortStr)
	if streamKey == "" || err != nil || remotePort < 1 || remotePort > 65535 {
		http.Error(w, "missing stream or invalid port", http.StatusBadRequest)
		return
	}
	session, err := t.sessions.begin(context.Background())
	if err != nil {
		http.Error(w, "transport unavailable", http.StatusServiceUnavailable)
		return
	}
	accepted := false
	defer func() {
		if !accepted {
			session.finish()
		}
	}()
	conn, rtpPort, err := session.listenUDPPair(t.ports)
	if err != nil {
		http.Error(w, "no available UDP ports", http.StatusServiceUnavailable)
		return
	}
	stream, pub, status, err := admitSignaledPublisher(session, t.server, t.hub, r, "gb-push", "gb28181", &avframe.MediaInfo{})
	if err != nil {
		http.Error(w, http.StatusText(status), status)
		return
	}
	if session.ctx.Err() != nil {
		http.Error(w, "transport unavailable", http.StatusServiceUnavailable)
		return
	}
	// Return local port for remote to send to
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprintf(w, "%d", rtpPort); err != nil {
		return
	}
	session.established = true
	accepted = true
	go func() { defer session.finish(); t.receivePush(stream, pub, conn) }()
}

// handlePullSignal handles the signaling request for an outgoing GB pull relay.
func (t *GBTransport) handlePullSignal(w http.ResponseWriter, r *http.Request) {
	if _, ok := readSignalingRequest(w, r); !ok {
		return
	}
	streamKey := r.URL.Query().Get("stream")
	remotePortStr := r.URL.Query().Get("port")
	if streamKey == "" || remotePortStr == "" {
		http.Error(w, "missing stream or port", http.StatusBadRequest)
		return
	}

	remotePort, err := strconv.Atoi(remotePortStr)
	if err != nil || remotePort < 1 || remotePort > 65535 {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}
	remoteSSRC := uint32(remotePort)

	stream, ok := t.hub.Find(streamKey)
	if !ok {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}
	snapshot := stream.StartupSnapshot()
	if !stream.IsPublisherGeneration(snapshot.Generation) {
		http.Error(w, "stream not found or no publisher", http.StatusNotFound)
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || net.ParseIP(host) == nil {
		http.Error(w, "invalid remote address", http.StatusBadRequest)
		return
	}
	session, err := t.sessions.begin(context.Background())
	if err != nil {
		http.Error(w, "transport unavailable", http.StatusServiceUnavailable)
		return
	}
	accepted := false
	defer func() {
		if !accepted {
			session.finish()
		}
	}()
	session.bindGeneration(snapshot)
	conn, err := session.dialUDP(t.ports, &net.UDPAddr{IP: net.ParseIP(host), Port: remotePort})
	if err != nil {
		http.Error(w, "UDP setup failed", http.StatusServiceUnavailable)
		return
	}
	if session.ctx.Err() != nil || !stream.IsPublisherGeneration(snapshot.Generation) {
		http.Error(w, "publisher retired during setup", http.StatusServiceUnavailable)
		return
	}
	if err := session.subscribe(stream, snapshot, "gb28181"); err != nil {
		http.Error(w, "subscriber admission failed", http.StatusServiceUnavailable)
		return
	}

	// Start background sender
	accepted = true
	go func() {
		defer session.finish()
		if err := t.sendPullConnection(session.ctx, stream, snapshot, conn, remoteSSRC); err != nil {
			slog.Warn("gb pull sender stopped", "module", "cluster", "error", err)
		}
	}()

	w.WriteHeader(http.StatusOK)
}

func (t *GBTransport) receivePush(stream *core.Stream, pub *originPublisher, conn *net.UDPConn) {

	demuxer := ps.NewDemuxer()
	var psBuf []byte
	buf := make([]byte, 2048)

	for {
		conn.SetReadDeadline(time.Now().Add(t.cfg.Timeout))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				slog.Info("gb push receiver timeout", "module", "cluster", "port", conn.LocalAddr())
			}
			return
		}

		if n < 12 {
			continue
		}

		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		psBuf, err = appendRelayPS(psBuf, pkt.Payload)
		if err != nil {
			return
		}
		if pkt.Marker {
			frames, _ := demuxer.Feed(psBuf)
			for _, frame := range frames {
				if frame.MediaType.IsAudio() {
					frame.Payload = bytes.Clone(frame.Payload)
				}
				if !stream.WriteFrameForPublisher(pub, frame) && stream.Publisher() != pub {
					return
				}
			}
			psBuf = psBuf[:0]
		}
	}
}

func (t *GBTransport) sendPull(stream *core.Stream, snapshot core.StreamStartupSnapshot, remoteAddr string, remotePort int) error {
	if remotePort < 1 || remotePort > 65535 {
		return fmt.Errorf("invalid remote UDP port: %d", remotePort)
	}
	session, err := t.sessions.begin(context.Background())
	if err != nil {
		return err
	}
	defer session.finish()
	session.bindGeneration(snapshot)
	host, _, _ := net.SplitHostPort(remoteAddr)
	remote := &net.UDPAddr{IP: net.ParseIP(host), Port: remotePort}

	conn, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		slog.Error("gb pull sender dial failed", "module", "cluster", "error", err)
		return fmt.Errorf("dial UDP: %w", err)
	}
	defer conn.Close()
	session.ownSocket(conn)
	return t.sendPullConnection(session.ctx, stream, snapshot, conn, uint32(remotePort))
}

func (t *GBTransport) sendPullConnection(ctx context.Context, stream *core.Stream, snapshot core.StreamStartupSnapshot, conn *net.UDPConn, ssrc uint32) error {
	muxer := ps.NewMuxer()
	var seq uint16
	var ts uint32
	var mu sync.Mutex

	sendFrame := func(frame *avframe.AVFrame) error {
		mu.Lock()
		defer mu.Unlock()
		return t.sendPSFrameObserved(ctx, conn, muxer, frame, &seq, &ts, ssrc)
	}

	if !stream.IsPublisherGeneration(snapshot.Generation) {
		return nil
	}
	if vsh := snapshot.VideoSequenceHeader; vsh != nil {
		if err := sendFrame(vsh); err != nil {
			return fmt.Errorf("send video sequence header: %w", err)
		}
	}
	for _, frame := range snapshot.ReplayFrames {
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return nil
		}
		if err := sendFrame(frame); err != nil {
			return fmt.Errorf("send replay frame: %w", err)
		}
	}

	reader := stream.RingBuffer().NewReaderAt(snapshot.LiveCursor)
	for {
		frame, ok, readErr := core.ReadFrameContext(ctx, reader)
		if readErr != nil {
			return fmt.Errorf("source ring continuity lost: %w", readErr)
		}
		if !ok {
			return nil
		}
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return nil
		}

		if err := sendFrame(frame); err != nil {
			slog.Warn("gb pull send error", "module", "cluster", "error", err)
			return fmt.Errorf("send frame: %w", err)
		}
	}
}
