package sipgateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
)

// CallSession manages a single SIP call with RTP bridging to/from a stream.
type CallSession struct {
	callID    string
	streamKey string
	codec     negotiatedCodec
	direction string // "inbound" or "outbound"

	rtpPort    int
	rtcpPort   int
	conn       *net.UDPConn
	rtcpConn   *net.UDPConn
	remoteAddr *net.UDPAddr

	stream    *core.Stream
	publisher *sipPublisher
	dialog    *dialogTeardown
	metrics   *gatewayMetrics

	lifecycleMu     sync.Mutex
	mu              sync.RWMutex
	state           CallState
	startedAt       time.Time
	lastError       string
	lastRTPUnixNano atomic.Int64
	rtpPacketsSent  atomic.Uint64
	rtpPacketsRecv  atomic.Uint64
	rtpBytesSent    atomic.Uint64
	rtpBytesRecv    atomic.Uint64
	rtpIdleTimeout  time.Duration
	onTerminate     func(*CallSession, CallState, error)
	established     atomic.Bool
	terminateOnce   sync.Once
	stopOnce        sync.Once
	rtcpSendOnce    sync.Once
	closed          chan struct{}
}

type dialogTeardown struct {
	dialog inviteDialog
	once   sync.Once
	err    error
}

func newDialogTeardown(dialog inviteDialog) *dialogTeardown {
	return &dialogTeardown{dialog: dialog}
}

func (d *dialogTeardown) teardown() error {
	if d == nil || d.dialog == nil {
		return nil
	}
	d.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		d.err = d.dialog.SendBYE(ctx)
		cancel()
		d.dialog.Close()
	})
	return d.err
}

func (d *dialogTeardown) endedByRemote() {
	if d == nil || d.dialog == nil {
		return
	}
	d.once.Do(func() {
		d.dialog.Close()
	})
}

type sipPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *sipPublisher) ID() string                    { return p.id }
func (p *sipPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *sipPublisher) Close() error                  { return nil }

func newCallSession(callID, streamKey string, codec negotiatedCodec, direction string, rtpPort, rtcpPort int) *CallSession {
	return &CallSession{
		callID:         callID,
		streamKey:      streamKey,
		codec:          codec,
		direction:      direction,
		rtpPort:        rtpPort,
		rtcpPort:       rtcpPort,
		state:          CallStateEstablishing,
		startedAt:      time.Now().UTC(),
		rtpIdleTimeout: 30 * time.Second,
		closed:         make(chan struct{}),
	}
}

func (cs *CallSession) startInbound(stream *core.Stream, remoteIP string, remotePort int) error {
	cs.lifecycleMu.Lock()
	defer cs.lifecycleMu.Unlock()
	select {
	case <-cs.closed:
		return errors.New("call session is terminated")
	default:
	}

	publisher := &sipPublisher{
		id: "sip-" + cs.callID,
		info: &avframe.MediaInfo{
			AudioCodec: cs.codec.Codec,
			SampleRate: cs.codec.ClockRate,
			Channels:   1,
		},
	}
	if err := stream.SetPublisher(publisher); err != nil {
		return fmt.Errorf("set stream publisher: %w", err)
	}
	cs.mu.Lock()
	cs.stream = stream
	cs.publisher = publisher
	cs.mu.Unlock()

	addr := &net.UDPAddr{Port: cs.rtpPort}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}

	cs.mu.Lock()
	cs.conn = conn
	if remoteIP != "" && remotePort > 0 {
		cs.remoteAddr = &net.UDPAddr{IP: net.ParseIP(remoteIP), Port: remotePort}
	}
	cs.state = CallStateActive
	cs.mu.Unlock()
	cs.established.Store(true)

	go cs.receiveLoop()
	return nil
}

func (cs *CallSession) startOutbound(stream *core.Stream, remoteIP string, remotePort int) error {
	cs.lifecycleMu.Lock()
	defer cs.lifecycleMu.Unlock()
	select {
	case <-cs.closed:
		return errors.New("call session is terminated")
	default:
	}

	remoteIPAddr := net.ParseIP(remoteIP)
	if remoteIPAddr == nil || remotePort <= 0 {
		return fmt.Errorf("invalid remote RTP address %q:%d", remoteIP, remotePort)
	}

	addr := &net.UDPAddr{Port: cs.rtpPort}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	rtcpConn, err := net.ListenUDP("udp", &net.UDPAddr{Port: cs.rtcpPort})
	if err != nil {
		_ = conn.Close()
		return err
	}

	cs.mu.Lock()
	cs.stream = stream
	cs.remoteAddr = &net.UDPAddr{IP: remoteIPAddr, Port: remotePort}
	cs.conn = conn
	cs.rtcpConn = rtcpConn
	cs.state = CallStateActive
	cs.mu.Unlock()
	cs.established.Store(true)

	go cs.sendLoop()
	go cs.receiveRTCPLoop()
	return nil
}

func (cs *CallSession) receiveRTCPLoop() {
	defer slog.Info("rtcp receive loop stopped", "module", "sipgateway", "call", cs.callID)

	buf := make([]byte, 1500)
	cs.mu.RLock()
	conn := cs.rtcpConn
	remoteAddr := cs.remoteAddr
	idleTimeout := cs.rtpIdleTimeout
	cs.mu.RUnlock()
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Second
	}
	lastValid := time.Now()

	for {
		if err := conn.SetReadDeadline(lastValid.Add(idleTimeout)); err != nil {
			cs.networkLost(err)
			return
		}
		n, sender, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-cs.closed:
				return
			default:
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				cs.networkLost(fmt.Errorf("RTCP liveness timeout after %s", idleTimeout))
				return
			}
			cs.networkLost(err)
			return
		}
		if remoteAddr != nil && remoteAddr.IP != nil && !sender.IP.Equal(remoteAddr.IP) {
			continue
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err != nil || len(packets) == 0 {
			continue
		}
		lastValid = time.Now()
	}
}

func (cs *CallSession) receiveLoop() {
	defer slog.Info("rtp receive loop stopped", "module", "sipgateway", "call", cs.callID)

	depacketizer := cs.newDepacketizer()
	if depacketizer.inner == nil {
		cs.networkLost(ErrCodecMismatch)
		return
	}
	buf := make([]byte, 2048)
	cs.mu.RLock()
	conn := cs.conn
	idleTimeout := cs.rtpIdleTimeout
	cs.mu.RUnlock()
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Second
	}

	for {
		if err := conn.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
			cs.networkLost(err)
			return
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-cs.closed:
				return
			default:
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				cs.networkLost(fmt.Errorf("RTP idle timeout after %s", idleTimeout))
				return
			}
			slog.Warn("rtp read error", "module", "sipgateway", "call", cs.callID, "error", err)
			cs.networkLost(err)
			return
		}

		if n < 12 {
			continue
		}

		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		cs.recordRTPReceived(n)

		frame, err := depacketizer.depacketize(&pkt)
		if err != nil || frame == nil {
			continue
		}

		cs.stream.WriteFrame(frame)
	}
}

func (cs *CallSession) sendLoop() {
	defer slog.Info("rtp send loop stopped", "module", "sipgateway", "call", cs.callID)

	session := rtp.NewSession(uint8(cs.codec.PT), uint32(cs.codec.ClockRate))
	packetizer := cs.newPacketizer()
	if packetizer.inner == nil {
		cs.networkLost(ErrCodecMismatch)
		return
	}
	cs.mu.RLock()
	stream := cs.stream
	conn := cs.conn
	remoteAddr := cs.remoteAddr
	cs.mu.RUnlock()

	if err := stream.AddSubscriber("sipgateway"); err != nil {
		cs.networkLost(err)
		return
	}
	defer stream.RemoveSubscriber("sipgateway")

	reader := stream.RingBuffer().NewReader()
	readCtx, cancelRead := context.WithCancel(context.Background())
	defer cancelRead()
	go func() {
		select {
		case <-cs.closed:
			cancelRead()
		case <-readCtx.Done():
		}
	}()

	for {
		frame, ok := reader.ReadContext(readCtx)
		if !ok {
			if stream.RingBuffer().IsClosed() {
				cs.ended()
				return
			}
			return
		}

		if frame.MediaType != avframe.MediaTypeAudio {
			continue
		}

		pkts, err := packetizer.packetize(frame)
		if err != nil || len(pkts) == 0 {
			continue
		}

		session.WrapPackets(pkts, frame.DTS)

		for _, pkt := range pkts {
			data, err := pkt.Marshal()
			if err != nil {
				continue
			}
			n, err := conn.WriteToUDP(data, remoteAddr)
			if err != nil {
				cs.networkLost(err)
				return
			}
			cs.recordRTPSent(n)
			cs.sendSenderReport(remoteAddr, pkt.Timestamp)
		}
	}
}

func (cs *CallSession) sendSenderReport(remoteAddr *net.UDPAddr, rtpTimestamp uint32) {
	if remoteAddr == nil || remoteAddr.Port >= 65535 {
		return
	}
	cs.rtcpSendOnce.Do(func() {
		cs.mu.RLock()
		conn := cs.rtcpConn
		cs.mu.RUnlock()
		if conn == nil {
			return
		}
		data, err := (&rtcp.SenderReport{
			SSRC:        0x4c465247,
			NTPTime:     uint64(time.Now().UnixNano()),
			RTPTime:     rtpTimestamp,
			PacketCount: uint32(cs.rtpPacketsSent.Load()),
			OctetCount:  uint32(cs.rtpBytesSent.Load()),
		}).Marshal()
		if err != nil {
			return
		}
		_, _ = conn.WriteToUDP(data, &net.UDPAddr{IP: remoteAddr.IP, Port: remoteAddr.Port + 1})
	})
}

// Close terminates a session and notifies its gateway owner exactly once.
func (cs *CallSession) Close() {
	cs.terminate(CallStateEnded, nil, true)
}

func (cs *CallSession) ended() {
	cs.terminate(CallStateEnded, nil, true)
}

func (cs *CallSession) networkLost(err error) {
	cs.terminate(CallStateNetworkLost, err, true)
}

func (cs *CallSession) terminate(state CallState, err error, notify bool) bool {
	terminated := false
	var callback func(*CallSession, CallState, error)
	cs.lifecycleMu.Lock()
	cs.terminateOnce.Do(func() {
		terminated = true
		cs.mu.Lock()
		cs.state = state
		if err != nil {
			cs.lastError = redactedTerminalError(err)
		}
		callback = cs.onTerminate
		cs.mu.Unlock()
		cs.stop()
		if state == CallStateNetworkLost && cs.metrics != nil {
			cs.metrics.networkFailures.Add(1)
		}
	})
	cs.lifecycleMu.Unlock()
	if terminated && notify && callback != nil {
		callback(cs, state, err)
	}
	return terminated
}

func (cs *CallSession) stop() {
	cs.stopOnce.Do(func() {
		close(cs.closed)
		cs.mu.RLock()
		conn := cs.conn
		rtcpConn := cs.rtcpConn
		cs.mu.RUnlock()
		if conn != nil {
			_ = conn.Close()
		}
		if rtcpConn != nil {
			_ = rtcpConn.Close()
		}
	})
}

func (cs *CallSession) recordRTPReceived(bytes int) {
	cs.rtpPacketsRecv.Add(1)
	cs.rtpBytesRecv.Add(uint64(bytes))
	cs.lastRTPUnixNano.Store(time.Now().UnixNano())
	if cs.metrics != nil {
		cs.metrics.rtpPacketsRecv.Add(1)
		cs.metrics.rtpBytesRecv.Add(uint64(bytes))
	}
}

func (cs *CallSession) recordRTPSent(bytes int) {
	cs.rtpPacketsSent.Add(1)
	cs.rtpBytesSent.Add(uint64(bytes))
	cs.lastRTPUnixNano.Store(time.Now().UnixNano())
	if cs.metrics != nil {
		cs.metrics.rtpPacketsSent.Add(1)
		cs.metrics.rtpBytesSent.Add(uint64(bytes))
	}
}

func (cs *CallSession) snapshot() CallSnapshot {
	cs.mu.RLock()
	remoteAddress := ""
	if cs.remoteAddr != nil {
		remoteAddress = cs.remoteAddr.String()
	}
	snapshot := CallSnapshot{
		CallID:        cs.callID,
		Direction:     cs.direction,
		StreamKey:     cs.streamKey,
		Codec:         cs.codec.EncodingName,
		RTPPort:       cs.rtpPort,
		RTCPPort:      cs.rtcpPort,
		RemoteAddress: remoteAddress,
		StartedAt:     cs.startedAt,
		State:         cs.state,
		LastError:     cs.lastError,
	}
	cs.mu.RUnlock()

	if lastRTP := cs.lastRTPUnixNano.Load(); lastRTP != 0 {
		snapshot.LastRTPAt = time.Unix(0, lastRTP).UTC()
	}
	snapshot.RTPPacketsSent = cs.rtpPacketsSent.Load()
	snapshot.RTPPacketsRecv = cs.rtpPacketsRecv.Load()
	snapshot.RTPBytesSent = cs.rtpBytesSent.Load()
	snapshot.RTPBytesRecv = cs.rtpBytesRecv.Load()
	return snapshot
}

type audioDepacketizer struct {
	codec avframe.CodecType
	inner interface {
		Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error)
	}
}

func (cs *CallSession) newDepacketizer() *audioDepacketizer {
	d := &audioDepacketizer{codec: cs.codec.Codec}
	switch cs.codec.Codec {
	case avframe.CodecG711U:
		d.inner = &rtp.G711Depacketizer{Codec: avframe.CodecG711U}
	case avframe.CodecG711A:
		d.inner = &rtp.G711Depacketizer{Codec: avframe.CodecG711A}
	case avframe.CodecOpus:
		d.inner = &rtp.OpusDepacketizer{}
	case avframe.CodecG722:
		d.inner = &rtp.G722Depacketizer{}
	}
	return d
}

func (d *audioDepacketizer) depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	if d.inner == nil {
		return nil, ErrCodecMismatch
	}
	return d.inner.Depacketize(pkt)
}

type audioPacketizer struct {
	inner interface {
		Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error)
	}
}

func (cs *CallSession) newPacketizer() *audioPacketizer {
	p := &audioPacketizer{}
	switch cs.codec.Codec {
	case avframe.CodecG711U, avframe.CodecG711A:
		p.inner = &rtp.G711Packetizer{}
	case avframe.CodecOpus:
		p.inner = &rtp.OpusPacketizer{}
	case avframe.CodecG722:
		p.inner = &rtp.G722Packetizer{}
	}
	return p
}

func (p *audioPacketizer) packetize(frame *avframe.AVFrame) ([]*pionrtp.Packet, error) {
	if p.inner == nil {
		return nil, ErrCodecMismatch
	}
	return p.inner.Packetize(frame, 1400)
}
