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
	"sync"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	"github.com/im-pingo/liveforge/pkg/portalloc"
	pionrtp "github.com/pion/rtp/v2"
)

// GBTransport implements RelayTransport for GB28181 PS-over-RTP relay.
// Uses SDP-over-HTTP signaling similar to RTPTransport, but encapsulates
// frames in MPEG-PS format within RTP packets.
type GBTransport struct {
	cfg   config.ClusterGBConfig
	ports *portalloc.PortAllocator
	hub   *core.StreamHub
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
		cfg:   cfg,
		ports: ports,
		hub:   s.StreamHub(),
	}

	// Register signaling handlers.
	pushPath := cfg.SignalingPath + "/push"
	pullPath := cfg.SignalingPath + "/pull"
	s.RegisterAPIHandler("POST "+pushPath, http.HandlerFunc(t.handlePushSignal))
	s.RegisterAPIHandler("POST "+pullPath, http.HandlerFunc(t.handlePullSignal))

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

	// Allocate local port pair
	rtpPort, _, err := t.ports.AllocatePair()
	if err != nil {
		return fmt.Errorf("allocate port pair: %w", err)
	}
	defer t.ports.Free(rtpPort, rtpPort+1)

	// Signal remote to start receiving
	sigURL := fmt.Sprintf("http://%s%s/push?stream=%s&port=%d",
		u.Host, t.cfg.SignalingPath, url.QueryEscape(u.Path), rtpPort)

	resp, err := http.Post(sigURL, "text/plain", nil) //nolint:gosec
	if err != nil {
		return fmt.Errorf("signaling request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("signaling rejected: %d %s", resp.StatusCode, string(body))
	}

	// Read remote port from response
	body, _ := io.ReadAll(resp.Body)
	remotePort, _ := strconv.Atoi(string(bytes.TrimSpace(body)))
	if remotePort == 0 {
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

	slog.Info("gb relay push connected", "module", "cluster", "target", targetURL, "remote_port", remotePort)

	muxer := ps.NewMuxer()
	var seq uint16
	var ts uint32
	ssrc := uint32(rtpPort) // simple SSRC

	// Send sequence headers first
	if vsh := stream.VideoSeqHeader(); vsh != nil {
		if err := t.sendPSFrame(conn, muxer, vsh, &seq, &ts, ssrc); err != nil {
			return fmt.Errorf("send video seq header: %w", err)
		}
	}

	reader := stream.RingBuffer().NewReader()
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		frame, ok := reader.TryRead()
		if !ok {
			if stream.RingBuffer().IsClosed() {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-stream.RingBuffer().Signal():
			}
			continue
		}

		if err := t.sendPSFrame(conn, muxer, frame, &seq, &ts, ssrc); err != nil {
			return fmt.Errorf("send frame: %w", err)
		}
	}
}

func (t *GBTransport) sendPSFrame(conn *net.UDPConn, muxer *ps.Muxer, frame *avframe.AVFrame, seq *uint16, ts *uint32, ssrc uint32) error {
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
		if _, err := conn.Write(data); err != nil {
			return fmt.Errorf("write RTP: %w", err)
		}
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

	// Allocate local port pair for receiving
	rtpPort, _, err := t.ports.AllocatePair()
	if err != nil {
		return fmt.Errorf("allocate port pair: %w", err)
	}
	defer t.ports.Free(rtpPort, rtpPort+1)

	// Signal remote to start sending
	sigURL := fmt.Sprintf("http://%s%s/pull?stream=%s&port=%d",
		u.Host, t.cfg.SignalingPath, url.QueryEscape(u.Path), rtpPort)

	resp, err := http.Post(sigURL, "text/plain", nil) //nolint:gosec
	if err != nil {
		return fmt.Errorf("signaling request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("signaling rejected: %d %s", resp.StatusCode, string(body))
	}

	slog.Info("gb relay pull started", "module", "cluster", "source", sourceURL, "local_port", rtpPort)

	// Listen for incoming RTP
	listenAddr := &net.UDPAddr{Port: rtpPort}
	conn, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen UDP: %w", err)
	}
	defer conn.Close()

	pub := &originPublisher{
		id:   fmt.Sprintf("gb-pull-%s", stream.Key()),
		info: &avframe.MediaInfo{},
	}
	if err := stream.SetPublisher(pub); err != nil {
		return fmt.Errorf("set publisher: %w", err)
	}
	defer stream.RemovePublisher()

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
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				return fmt.Errorf("receive timeout")
			}
			return fmt.Errorf("read UDP: %w", err)
		}

		if n < 12 {
			continue
		}

		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		psBuf = append(psBuf, pkt.Payload...)

		if pkt.Marker {
			frames, err := demuxer.Feed(psBuf)
			if err != nil {
				slog.Debug("ps demux error in gb pull", "module", "cluster", "error", err)
			}
			for _, frame := range frames {
				if pub.info.VideoCodec == 0 && frame.MediaType == avframe.MediaTypeVideo {
					pub.info.VideoCodec = frame.Codec
				}
				if pub.info.AudioCodec == 0 && frame.MediaType == avframe.MediaTypeAudio {
					pub.info.AudioCodec = frame.Codec
				}
				stream.WriteFrame(frame)
			}
			psBuf = psBuf[:0]
		}
	}
}

func (t *GBTransport) Close() error { return nil }

// handlePushSignal handles the signaling request for an incoming GB push relay.
func (t *GBTransport) handlePushSignal(w http.ResponseWriter, r *http.Request) {
	streamKey := r.URL.Query().Get("stream")
	remotePortStr := r.URL.Query().Get("port")
	if streamKey == "" || remotePortStr == "" {
		http.Error(w, "missing stream or port", http.StatusBadRequest)
		return
	}

	rtpPort, _, err := t.ports.AllocatePair()
	if err != nil {
		http.Error(w, "port allocation failed", http.StatusServiceUnavailable)
		return
	}

	// Start background receiver for the pushed stream
	stream, _ := t.hub.GetOrCreate(streamKey)
	go t.receivePush(stream, rtpPort)

	// Return local port for remote to send to
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "%d", rtpPort)
}

// handlePullSignal handles the signaling request for an outgoing GB pull relay.
func (t *GBTransport) handlePullSignal(w http.ResponseWriter, r *http.Request) {
	streamKey := r.URL.Query().Get("stream")
	remotePortStr := r.URL.Query().Get("port")
	if streamKey == "" || remotePortStr == "" {
		http.Error(w, "missing stream or port", http.StatusBadRequest)
		return
	}

	remotePort, _ := strconv.Atoi(remotePortStr)
	if remotePort == 0 {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}

	stream, ok := t.hub.Find(streamKey)
	if !ok {
		http.Error(w, "stream not found", http.StatusNotFound)
		return
	}

	// Start background sender
	go t.sendPull(stream, r.RemoteAddr, remotePort)

	w.WriteHeader(http.StatusOK)
}

func (t *GBTransport) receivePush(stream *core.Stream, rtpPort int) {
	defer t.ports.Free(rtpPort, rtpPort+1)

	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: rtpPort})
	if err != nil {
		slog.Error("gb push receiver listen failed", "module", "cluster", "error", err)
		return
	}
	defer conn.Close()

	pub := &originPublisher{
		id:   fmt.Sprintf("gb-push-%s", stream.Key()),
		info: &avframe.MediaInfo{},
	}
	if err := stream.SetPublisher(pub); err != nil {
		slog.Error("gb push set publisher failed", "module", "cluster", "error", err)
		return
	}
	defer stream.RemovePublisher()

	demuxer := ps.NewDemuxer()
	var psBuf []byte
	buf := make([]byte, 2048)

	for {
		conn.SetReadDeadline(time.Now().Add(t.cfg.Timeout))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				slog.Info("gb push receiver timeout", "module", "cluster", "port", rtpPort)
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

		psBuf = append(psBuf, pkt.Payload...)
		if pkt.Marker {
			frames, _ := demuxer.Feed(psBuf)
			for _, frame := range frames {
				if pub.info.VideoCodec == 0 && frame.MediaType == avframe.MediaTypeVideo {
					pub.info.VideoCodec = frame.Codec
				}
				stream.WriteFrame(frame)
			}
			psBuf = psBuf[:0]
		}
	}
}

func (t *GBTransport) sendPull(stream *core.Stream, remoteAddr string, remotePort int) {
	host, _, _ := net.SplitHostPort(remoteAddr)
	remote := &net.UDPAddr{IP: net.ParseIP(host), Port: remotePort}

	conn, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		slog.Error("gb pull sender dial failed", "module", "cluster", "error", err)
		return
	}
	defer conn.Close()

	muxer := ps.NewMuxer()
	var seq uint16
	var ts uint32
	ssrc := uint32(remotePort)
	var mu sync.Mutex

	sendFrame := func(frame *avframe.AVFrame) error {
		mu.Lock()
		defer mu.Unlock()
		return t.sendPSFrame(conn, muxer, frame, &seq, &ts, ssrc)
	}

	if vsh := stream.VideoSeqHeader(); vsh != nil {
		sendFrame(vsh) //nolint:errcheck
	}

	reader := stream.RingBuffer().NewReader()
	for {
		frame, ok := reader.TryRead()
		if !ok {
			if stream.RingBuffer().IsClosed() {
				return
			}
			<-stream.RingBuffer().Signal()
			continue
		}

		if err := sendFrame(frame); err != nil {
			slog.Warn("gb pull send error", "module", "cluster", "error", err)
			return
		}
	}
}
