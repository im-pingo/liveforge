package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/sdp"
	pionrtp "github.com/pion/rtp/v2"
)

// portAllocator manages a pool of UDP ports for RTP sessions.
type portAllocator struct {
	mu        sync.Mutex
	available map[int]struct{}
	minPort   int
	maxPort   int
}

// newPortAllocator creates a port allocator from a range string like "20000-20100".
func newPortAllocator(portRange string) (*portAllocator, error) {
	parts := strings.SplitN(portRange, "-", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid port range %q: expected min-max", portRange)
	}
	minPort, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return nil, fmt.Errorf("invalid min port %q: %w", parts[0], err)
	}
	maxPort, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return nil, fmt.Errorf("invalid max port %q: %w", parts[1], err)
	}
	if minPort > maxPort {
		return nil, fmt.Errorf("invalid port range: min %d > max %d", minPort, maxPort)
	}
	if minPort < 1 || maxPort > 65535 {
		return nil, fmt.Errorf("port range out of bounds: %d-%d", minPort, maxPort)
	}

	avail := make(map[int]struct{}, maxPort-minPort+1)
	for p := minPort; p <= maxPort; p++ {
		avail[p] = struct{}{}
	}
	return &portAllocator{
		available: avail,
		minPort:   minPort,
		maxPort:   maxPort,
	}, nil
}

// Allocate returns an available port or error if exhausted.
func (pa *portAllocator) Allocate() (int, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	for port := range pa.available {
		delete(pa.available, port)
		return port, nil
	}
	return 0, fmt.Errorf("no available RTP ports in range %d-%d", pa.minPort, pa.maxPort)
}

// Free returns a port to the pool.
func (pa *portAllocator) Free(port int) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	if port >= pa.minPort && port <= pa.maxPort {
		pa.available[port] = struct{}{}
	}
}

// RTPTransport implements RelayTransport for direct RTP relay with SDP-over-HTTP signaling.
type RTPTransport struct {
	cfg   config.ClusterRTPConfig
	ports *portAllocator
	hub   *core.StreamHub
}

// NewRTPTransport creates a new RTP relay transport.
// It registers SDP signaling HTTP handlers on the server.
func NewRTPTransport(cfg config.ClusterRTPConfig, s *core.Server) *RTPTransport {
	if cfg.PortRange == "" {
		cfg.PortRange = "20000-20100"
	}
	if cfg.SignalingPath == "" {
		cfg.SignalingPath = "/api/relay"
	}
	if cfg.RTCPInterval == 0 {
		cfg.RTCPInterval = 5 * time.Second
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}

	ports, err := newPortAllocator(cfg.PortRange)
	if err != nil {
		slog.Warn("rtp port allocator init failed, using defaults", "module", "cluster", "error", err)
		ports, _ = newPortAllocator("20000-20100")
	}

	t := &RTPTransport{
		cfg:   cfg,
		ports: ports,
		hub:   s.StreamHub(),
	}

	// Register signaling handlers.
	pushPath := cfg.SignalingPath + "/push"
	pullPath := cfg.SignalingPath + "/pull"
	s.RegisterAPIHandler(pushPath, http.HandlerFunc(t.handleSignalingPush))
	s.RegisterAPIHandler(pullPath, http.HandlerFunc(t.handleSignalingPull))

	slog.Info("rtp transport initialized", "module", "cluster",
		"port_range", cfg.PortRange, "signaling_path", cfg.SignalingPath)
	return t
}

func (t *RTPTransport) Scheme() string { return "rtp" }

// Push sends stream data via RTP to a remote node.
// It performs SDP signaling via HTTP POST and then sends RTP packets over UDP.
func (t *RTPTransport) Push(ctx context.Context, targetURL string, stream *core.Stream) error {
	host, streamKey, err := parseRTPURL(targetURL)
	if err != nil {
		return fmt.Errorf("parse RTP URL: %w", err)
	}

	pub := stream.Publisher()
	if pub == nil {
		return fmt.Errorf("stream has no publisher")
	}
	mi := pub.MediaInfo()

	// Build SDP offer from stream media info.
	sd := sdp.BuildFromMediaInfo(mi, targetURL, "0.0.0.0")
	offerSDP := sd.Marshal()

	// POST SDP offer to signaling endpoint.
	sigURL := fmt.Sprintf("http://%s%s/push?stream=%s", host, t.cfg.SignalingPath, url.QueryEscape(streamKey))
	answerBody, err := postSDP(ctx, sigURL, offerSDP)
	if err != nil {
		return fmt.Errorf("signaling POST to %s: %w", sigURL, err)
	}

	// Parse SDP answer to get remote address.
	answerSD, err := sdp.Parse(answerBody)
	if err != nil {
		return fmt.Errorf("parse SDP answer: %w", err)
	}

	remoteAddr, err := extractRemoteAddr(answerSD)
	if err != nil {
		return fmt.Errorf("extract remote address: %w", err)
	}

	// Allocate local UDP port and dial remote.
	localPort, err := t.ports.Allocate()
	if err != nil {
		return fmt.Errorf("allocate port: %w", err)
	}
	defer t.ports.Free(localPort)

	localAddr := &net.UDPAddr{Port: localPort}
	udpConn, err := net.DialUDP("udp", localAddr, remoteAddr)
	if err != nil {
		return fmt.Errorf("dial UDP %s: %w", remoteAddr, err)
	}

	// Close connection on context cancel.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			udpConn.Close()
		case <-done:
			udpConn.Close()
		}
	}()

	slog.Info("rtp relay push connected", "module", "cluster",
		"target", targetURL, "remote", remoteAddr, "local_port", localPort)

	// Build packetizers and sessions per codec.
	var videoSession, audioSession *pkgrtp.Session
	var videoPkt, audioPkt pkgrtp.Packetizer

	if mi.VideoCodec != 0 {
		videoPkt, _ = pkgrtp.NewPacketizer(mi.VideoCodec)
		videoSession = pkgrtp.NewSession(96, 90000)
	}
	if mi.AudioCodec != 0 {
		audioPkt, _ = pkgrtp.NewPacketizer(mi.AudioCodec)
		clockRate := uint32(mi.SampleRate)
		if clockRate == 0 {
			clockRate = 48000
		}
		audioSession = pkgrtp.NewSession(97, clockRate)
	}

	// Start RTCP SR goroutine.
	rtcpDone := make(chan struct{})
	go func() {
		defer close(rtcpDone)
		ticker := time.NewTicker(t.cfg.RTCPInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if videoSession != nil {
					sr := pkgrtp.BuildSR(videoSession.SSRC, 0, 0, 0, 0)
					udpConn.Write(sr)
				}
				if audioSession != nil {
					sr := pkgrtp.BuildSR(audioSession.SSRC, 0, 0, 0, 0)
					udpConn.Write(sr)
				}
			}
		}
	}()

	reader := stream.RingBuffer().NewReader()
	for {
		select {
		case <-ctx.Done():
			sendBYE(udpConn, videoSession, audioSession)
			return nil
		default:
		}

		frame, ok := reader.TryRead()
		if !ok {
			if stream.RingBuffer().IsClosed() {
				sendBYE(udpConn, videoSession, audioSession)
				return nil
			}
			select {
			case <-ctx.Done():
				sendBYE(udpConn, videoSession, audioSession)
				return nil
			case <-stream.RingBuffer().Signal():
			}
			continue
		}

		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}

		var pkt pkgrtp.Packetizer
		var session *pkgrtp.Session

		if frame.MediaType.IsVideo() && videoPkt != nil {
			pkt = videoPkt
			session = videoSession
		} else if frame.MediaType.IsAudio() && audioPkt != nil {
			pkt = audioPkt
			session = audioSession
		} else {
			continue
		}

		rtpPackets, err := pkt.Packetize(frame, pkgrtp.DefaultMTU)
		if err != nil {
			continue
		}
		session.WrapPackets(rtpPackets, frame.DTS)

		for _, p := range rtpPackets {
			raw, err := p.Marshal()
			if err != nil {
				continue
			}
			if _, err := udpConn.Write(raw); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("udp write: %w", err)
			}
		}
	}
}

// Pull receives RTP data from a remote node into a local stream.
func (t *RTPTransport) Pull(ctx context.Context, sourceURL string, stream *core.Stream) error {
	host, streamKey, err := parseRTPURL(sourceURL)
	if err != nil {
		return fmt.Errorf("parse RTP URL: %w", err)
	}

	// Allocate local UDP port for receiving.
	localPort, err := t.ports.Allocate()
	if err != nil {
		return fmt.Errorf("allocate port: %w", err)
	}
	defer t.ports.Free(localPort)

	// Build a minimal SDP offer with our listen port.
	offerSD := &sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username: "-", SessionID: "0", SessionVersion: "1",
			NetType: "IN", AddrType: "IP4", Address: "0.0.0.0",
		},
		Name: "LiveForge Pull",
		Connection: &sdp.Connection{
			NetType: "IN", AddrType: "IP4", Address: "0.0.0.0",
		},
		Timing: sdp.Timing{Start: 0, Stop: 0},
		Media: []*sdp.MediaDescription{
			{
				Type:    "video",
				Port:    localPort,
				Proto:   "RTP/AVP",
				Formats: []int{96, 97, 98, 99, 100},
				Attributes: []sdp.Attribute{
					{Key: "recvonly"},
				},
			},
			{
				Type:    "audio",
				Port:    localPort,
				Proto:   "RTP/AVP",
				Formats: []int{101, 111, 0, 8},
				Attributes: []sdp.Attribute{
					{Key: "recvonly"},
				},
			},
		},
	}
	offerSDP := offerSD.Marshal()

	// POST SDP offer to signaling endpoint.
	sigURL := fmt.Sprintf("http://%s%s/pull?stream=%s", host, t.cfg.SignalingPath, url.QueryEscape(streamKey))
	answerBody, err := postSDP(ctx, sigURL, offerSDP)
	if err != nil {
		return fmt.Errorf("signaling POST to %s: %w", sigURL, err)
	}

	// Parse SDP answer.
	answerSD, err := sdp.Parse(answerBody)
	if err != nil {
		return fmt.Errorf("parse SDP answer: %w", err)
	}

	mi := sdpToMediaInfo(answerSD)
	ptMap := buildPTMap(answerSD)

	// Listen on local UDP port.
	listenAddr := &net.UDPAddr{Port: localPort}
	udpConn, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen UDP :%d: %w", localPort, err)
	}

	// Close connection on context cancel.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			udpConn.Close()
		case <-done:
			udpConn.Close()
		}
	}()

	slog.Info("rtp relay pull listening", "module", "cluster",
		"source", sourceURL, "local_port", localPort)

	pub := &originPublisher{
		id:   fmt.Sprintf("rtp-pull-%s", stream.Key()),
		info: mi,
	}
	if err := stream.SetPublisher(pub); err != nil {
		return fmt.Errorf("set publisher: %w", err)
	}
	defer stream.RemovePublisher()

	// Build depacketizers.
	depacketizers := make(map[uint8]pkgrtp.Depacketizer)
	for pt, codec := range ptMap {
		dp, err := pkgrtp.NewDepacketizer(codec)
		if err == nil {
			depacketizers[pt] = dp
		}
	}

	// Track remote sender address and SSRC for RTCP RR.
	var mu sync.Mutex
	var senderAddr *net.UDPAddr
	var senderSSRC uint32
	localSSRC := uint32(0x12345678) // arbitrary SSRC for receiver
	var highestSeq uint32

	// RTCP RR goroutine — sends Receiver Reports to remote sender.
	go func() {
		ticker := time.NewTicker(t.cfg.RTCPInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				mu.Lock()
				addr := senderAddr
				ssrc := senderSSRC
				seq := highestSeq
				mu.Unlock()
				if addr != nil {
					rr := pkgrtp.BuildRR(localSSRC, []pkgrtp.ReceptionReport{{
						SSRC:       ssrc,
						HighestSeq: seq,
					}})
					udpConn.WriteTo(rr, addr)
				}
			}
		}
	}()

	sendPullBYE := func() {
		mu.Lock()
		addr := senderAddr
		mu.Unlock()
		if addr != nil {
			bye := pkgrtp.BuildBYE(localSSRC)
			udpConn.WriteTo(bye, addr)
		}
	}

	buf := make([]byte, 2048)
	for {
		udpConn.SetReadDeadline(time.Now().Add(t.cfg.Timeout))
		n, remoteAddr, err := udpConn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				sendPullBYE()
				return nil
			}
			// Check if timeout (no data received).
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				slog.Info("rtp relay pull timeout", "module", "cluster",
					"source", sourceURL)
				sendPullBYE()
				return nil
			}
			return fmt.Errorf("udp read: %w", err)
		}

		if n == 0 {
			continue
		}

		// Track sender address for RTCP RR.
		mu.Lock()
		if senderAddr == nil {
			senderAddr = remoteAddr
		}
		mu.Unlock()

		// Check if RTCP packet (PT 200-204 range with V=2).
		if n >= 4 && (buf[0]>>6) == 2 {
			pt := buf[1]
			if pt >= 200 && pt <= 204 {
				continue // Skip RTCP packets.
			}
		}

		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		// Track sender SSRC and highest sequence number.
		mu.Lock()
		senderSSRC = pkt.SSRC
		if uint32(pkt.SequenceNumber) > highestSeq {
			highestSeq = uint32(pkt.SequenceNumber)
		}
		mu.Unlock()

		dp, ok := depacketizers[pkt.PayloadType]
		if !ok {
			continue
		}

		frame, err := dp.Depacketize(&pkt)
		if err != nil || frame == nil {
			continue
		}

		stream.WriteFrame(frame)
	}
}

func (t *RTPTransport) Close() error { return nil }

// handleSignalingPush handles POST {signaling_path}/push?stream={key}.
// A remote node wants to push RTP to us. We allocate a local UDP port,
// parse their SDP offer, and respond with an SDP answer containing our listen address.
func (t *RTPTransport) handleSignalingPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	streamKey := r.URL.Query().Get("stream")
	if streamKey == "" {
		http.Error(w, "missing stream parameter", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	offerSD, err := sdp.Parse(body)
	if err != nil {
		http.Error(w, "parse SDP: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Check that at least one offered codec is supported.
	if !hasAnySupportedCodec(offerSD) {
		http.Error(w, ErrCodecMismatch.Error(), http.StatusNotAcceptable)
		return
	}

	// Verify stream exists before allocating resources.
	if _, ok := t.hub.Find(streamKey); !ok {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}

	// Allocate local port for receiving RTP.
	localPort, err := t.ports.Allocate()
	if err != nil {
		http.Error(w, "no available ports", http.StatusServiceUnavailable)
		return
	}

	// Build SDP answer with our listen address.
	answerSD := buildAnswerSDP(offerSD, localPort, getLocalIP())

	slog.Info("rtp signaling push accepted", "module", "cluster",
		"stream", streamKey, "local_port", localPort)

	// Start a goroutine to receive RTP and write to stream.
	go t.receiveRTP(streamKey, localPort, offerSD)

	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusOK)
	w.Write(answerSD.Marshal())
}

// handleSignalingPull handles POST {signaling_path}/pull?stream={key}.
// A remote node wants to pull RTP from us. We look up the stream,
// parse their SDP offer, and start sending RTP to them.
func (t *RTPTransport) handleSignalingPull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	streamKey := r.URL.Query().Get("stream")
	if streamKey == "" {
		http.Error(w, "missing stream parameter", http.StatusBadRequest)
		return
	}

	stream, ok := t.hub.Find(streamKey)
	if !ok || stream.Publisher() == nil {
		http.Error(w, "stream not found or no publisher", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	offerSD, err := sdp.Parse(body)
	if err != nil {
		http.Error(w, "parse SDP: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Check codec compatibility first.
	if !hasAnySupportedCodec(offerSD) {
		http.Error(w, ErrCodecMismatch.Error(), http.StatusNotAcceptable)
		return
	}

	// Get remote listen address from offer.
	remoteAddr, err := extractRemoteAddr(offerSD)
	if err != nil {
		http.Error(w, "extract remote address: "+err.Error(), http.StatusBadRequest)
		return
	}

	mi := stream.Publisher().MediaInfo()

	// Build answer SDP from our stream info.
	answerSD := sdp.BuildFromMediaInfo(mi, "", getLocalIP())

	slog.Info("rtp signaling pull accepted", "module", "cluster",
		"stream", streamKey, "remote", remoteAddr)

	// Start a goroutine to send RTP to the remote node.
	go t.sendRTP(stream, remoteAddr)

	w.Header().Set("Content-Type", "application/sdp")
	w.WriteHeader(http.StatusOK)
	w.Write(answerSD.Marshal())
}

// receiveRTP listens on localPort for incoming RTP and writes frames to a stream.
func (t *RTPTransport) receiveRTP(streamKey string, localPort int, offerSD *sdp.SessionDescription) {
	defer t.ports.Free(localPort)

	listenAddr := &net.UDPAddr{Port: localPort}
	udpConn, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		slog.Warn("rtp receive: listen failed", "module", "cluster",
			"port", localPort, "error", err)
		return
	}
	defer udpConn.Close()

	stream, ok := t.hub.Find(streamKey)
	if !ok {
		slog.Warn("rtp receive: stream not found", "module", "cluster", "stream", streamKey)
		return
	}

	// Set up publisher from offered codecs.
	mi := sdpToMediaInfo(offerSD)
	ptMap := buildPTMap(offerSD)

	pub := &originPublisher{
		id:   fmt.Sprintf("rtp-push-%s", streamKey),
		info: mi,
	}
	if err := stream.SetPublisher(pub); err != nil {
		slog.Warn("rtp receive: set publisher failed", "module", "cluster",
			"stream", streamKey, "error", err)
		return
	}
	defer stream.RemovePublisher()

	// Build depacketizers.
	depacketizers := make(map[uint8]pkgrtp.Depacketizer)
	for pt, codec := range ptMap {
		dp, err := pkgrtp.NewDepacketizer(codec)
		if err == nil {
			depacketizers[pt] = dp
		}
	}

	var senderAddr *net.UDPAddr
	localSSRC := uint32(0x87654321)

	buf := make([]byte, 2048)
	for {
		udpConn.SetReadDeadline(time.Now().Add(t.cfg.Timeout))
		n, remoteAddr, err := udpConn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				slog.Info("rtp receive timeout", "module", "cluster", "stream", streamKey)
			}
			// Send BYE if we know the sender.
			if senderAddr != nil {
				bye := pkgrtp.BuildBYE(localSSRC)
				udpConn.WriteTo(bye, senderAddr)
			}
			return
		}

		if n < 4 {
			continue
		}

		// Track sender address.
		if senderAddr == nil {
			senderAddr = remoteAddr
		}

		// Skip RTCP.
		if (buf[0]>>6) == 2 && buf[1] >= 200 && buf[1] <= 204 {
			continue
		}

		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		dp, ok := depacketizers[pkt.PayloadType]
		if !ok {
			continue
		}

		frame, err := dp.Depacketize(&pkt)
		if err != nil || frame == nil {
			continue
		}

		stream.WriteFrame(frame)
	}
}

// sendRTP reads from a stream and sends RTP to the remote address.
func (t *RTPTransport) sendRTP(stream *core.Stream, remoteAddr *net.UDPAddr) {
	localPort, err := t.ports.Allocate()
	if err != nil {
		slog.Warn("rtp send: allocate port failed", "module", "cluster", "error", err)
		return
	}
	defer t.ports.Free(localPort)

	localAddr := &net.UDPAddr{Port: localPort}
	udpConn, err := net.DialUDP("udp", localAddr, remoteAddr)
	if err != nil {
		slog.Warn("rtp send: dial failed", "module", "cluster", "error", err)
		return
	}
	defer udpConn.Close()

	pub := stream.Publisher()
	if pub == nil {
		return
	}
	mi := pub.MediaInfo()

	var videoSession, audioSession *pkgrtp.Session
	var videoPkt, audioPkt pkgrtp.Packetizer

	if mi.VideoCodec != 0 {
		videoPkt, _ = pkgrtp.NewPacketizer(mi.VideoCodec)
		videoSession = pkgrtp.NewSession(96, 90000)
	}
	if mi.AudioCodec != 0 {
		audioPkt, _ = pkgrtp.NewPacketizer(mi.AudioCodec)
		clockRate := uint32(mi.SampleRate)
		if clockRate == 0 {
			clockRate = 48000
		}
		audioSession = pkgrtp.NewSession(97, clockRate)
	}

	// RTCP SR goroutine for server-side send.
	srDone := make(chan struct{})
	go func() {
		defer close(srDone)
		ticker := time.NewTicker(t.cfg.RTCPInterval)
		defer ticker.Stop()
		for {
			select {
			case <-srDone:
				return
			case <-ticker.C:
				if videoSession != nil {
					sr := pkgrtp.BuildSR(videoSession.SSRC, 0, 0, 0, 0)
					udpConn.Write(sr)
				}
				if audioSession != nil {
					sr := pkgrtp.BuildSR(audioSession.SSRC, 0, 0, 0, 0)
					udpConn.Write(sr)
				}
			}
		}
	}()

	reader := stream.RingBuffer().NewReader()
	for {
		frame, ok := reader.TryRead()
		if !ok {
			if stream.RingBuffer().IsClosed() {
				sendBYE(udpConn, videoSession, audioSession)
				return
			}
			select {
			case <-stream.RingBuffer().Signal():
			case <-time.After(t.cfg.Timeout):
				sendBYE(udpConn, videoSession, audioSession)
				return
			}
			continue
		}

		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}

		var pkt pkgrtp.Packetizer
		var session *pkgrtp.Session

		if frame.MediaType.IsVideo() && videoPkt != nil {
			pkt = videoPkt
			session = videoSession
		} else if frame.MediaType.IsAudio() && audioPkt != nil {
			pkt = audioPkt
			session = audioSession
		} else {
			continue
		}

		rtpPackets, err := pkt.Packetize(frame, pkgrtp.DefaultMTU)
		if err != nil {
			continue
		}
		session.WrapPackets(rtpPackets, frame.DTS)

		for _, p := range rtpPackets {
			raw, err := p.Marshal()
			if err != nil {
				continue
			}
			if _, err := udpConn.Write(raw); err != nil {
				return
			}
		}
	}
}

// --- Helper functions ---

// parseRTPURL parses rtp://host:port/streamKey into host:port and stream key.
func parseRTPURL(rawURL string) (host, streamKey string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid RTP URL: %w", err)
	}
	if u.Scheme != "rtp" {
		return "", "", fmt.Errorf("unsupported scheme %q, want rtp", u.Scheme)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("missing host in RTP URL")
	}
	host = u.Host
	if u.Path == "" || u.Path == "/" {
		return "", "", fmt.Errorf("missing stream key in RTP URL")
	}
	streamKey = strings.TrimPrefix(u.Path, "/")
	return host, streamKey, nil
}

// postSDP posts SDP data to a URL and returns the response body.
func postSDP(ctx context.Context, sigURL string, sdpData []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sigURL, bytes.NewReader(sdpData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/sdp")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP POST: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusNotAcceptable {
		return nil, ErrCodecMismatch
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("signaling error: HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// extractRemoteAddr extracts the UDP address from an SDP session description.
// It uses the c= line address and the first m= line port.
func extractRemoteAddr(sd *sdp.SessionDescription) (*net.UDPAddr, error) {
	addr := "0.0.0.0"
	port := 0

	// Session-level connection.
	if sd.Connection != nil && sd.Connection.Address != "" {
		addr = sd.Connection.Address
	}

	// First media section.
	if len(sd.Media) > 0 {
		m := sd.Media[0]
		port = m.Port
		// Media-level connection overrides session-level.
		if m.Connection != nil && m.Connection.Address != "" {
			addr = m.Connection.Address
		}
	}

	if port == 0 {
		return nil, fmt.Errorf("no port found in SDP")
	}

	ip := net.ParseIP(addr)
	if ip == nil {
		// Try resolving hostname.
		ips, err := net.LookupIP(addr)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("resolve address %q: %w", addr, err)
		}
		ip = ips[0]
	}

	return &net.UDPAddr{IP: ip, Port: port}, nil
}

// buildAnswerSDP creates an SDP answer with the given local port and address.
func buildAnswerSDP(offer *sdp.SessionDescription, localPort int, localAddr string) *sdp.SessionDescription {
	answer := &sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username: "-", SessionID: "0", SessionVersion: "1",
			NetType: "IN", AddrType: "IP4", Address: localAddr,
		},
		Name: "LiveForge RTP Answer",
		Connection: &sdp.Connection{
			NetType: "IN", AddrType: "IP4", Address: localAddr,
		},
		Timing: sdp.Timing{Start: 0, Stop: 0},
	}

	// Mirror offered media sections with our port.
	for _, om := range offer.Media {
		md := &sdp.MediaDescription{
			Type:       om.Type,
			Port:       localPort,
			Proto:      om.Proto,
			Formats:    om.Formats,
			Attributes: om.Attributes,
		}
		answer.Media = append(answer.Media, md)
	}

	return answer
}

// hasAnySupportedCodec checks if the SDP has at least one codec we can handle.
func hasAnySupportedCodec(sd *sdp.SessionDescription) bool {
	for _, md := range sd.Media {
		for _, pt := range md.Formats {
			rtpMap := md.RTPMap(pt)
			if rtpMap == nil {
				continue
			}
			codec := encodingNameToCodec(rtpMap.EncodingName)
			if codec != 0 {
				return true
			}
		}
	}
	return false
}

// sendBYE sends RTCP BYE for all active sessions.
func sendBYE(conn *net.UDPConn, sessions ...*pkgrtp.Session) {
	for _, s := range sessions {
		if s != nil {
			bye := pkgrtp.BuildBYE(s.SSRC)
			conn.Write(bye)
		}
	}
}

// getLocalIP returns the local machine's first non-loopback IP address.
func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "0.0.0.0"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "0.0.0.0"
}

// defaultClusterRTPConfig returns a config with defaults for testing.
func defaultClusterRTPConfig() config.ClusterRTPConfig {
	return config.ClusterRTPConfig{
		PortRange:     "20000-20100",
		SignalingPath: "/api/relay",
		RTCPInterval:  5 * time.Second,
		Timeout:       15 * time.Second,
	}
}
