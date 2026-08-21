package sipgateway

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/rtp"
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
	remoteAddr *net.UDPAddr

	stream    *core.Stream
	publisher *sipPublisher

	mu     sync.Mutex
	closed chan struct{}
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
		callID:    callID,
		streamKey: streamKey,
		codec:     codec,
		direction: direction,
		rtpPort:   rtpPort,
		rtcpPort:  rtcpPort,
		closed:    make(chan struct{}),
	}
}

func (cs *CallSession) startInbound(stream *core.Stream, remoteIP string, remotePort int) error {
	cs.stream = stream

	cs.publisher = &sipPublisher{
		id: "sip-" + cs.callID,
		info: &avframe.MediaInfo{
			AudioCodec: cs.codec.Codec,
			SampleRate: cs.codec.ClockRate,
			Channels:   1,
		},
	}
	stream.SetPublisher(cs.publisher)

	addr := &net.UDPAddr{Port: cs.rtpPort}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	cs.conn = conn

	if remoteIP != "" && remotePort > 0 {
		cs.remoteAddr = &net.UDPAddr{IP: net.ParseIP(remoteIP), Port: remotePort}
	}

	go cs.receiveLoop()
	return nil
}

func (cs *CallSession) startOutbound(stream *core.Stream, remoteIP string, remotePort int) error {
	cs.stream = stream
	cs.remoteAddr = &net.UDPAddr{IP: net.ParseIP(remoteIP), Port: remotePort}

	addr := &net.UDPAddr{Port: cs.rtpPort}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	cs.conn = conn

	go cs.sendLoop()
	return nil
}

func (cs *CallSession) receiveLoop() {
	defer slog.Info("rtp receive loop stopped", "module", "sipgateway", "call", cs.callID)

	depacketizer := cs.newDepacketizer()
	buf := make([]byte, 2048)

	for {
		cs.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, _, err := cs.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-cs.closed:
				return
			default:
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			slog.Warn("rtp read error", "module", "sipgateway", "call", cs.callID, "error", err)
			return
		}

		if n < 12 {
			continue
		}

		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

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

	cs.stream.AddSubscriber("sipgateway")
	defer cs.stream.RemoveSubscriber("sipgateway")

	reader := cs.stream.RingBuffer().NewReader()
	defer reader.Close()
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
			cs.conn.WriteToUDP(data, cs.remoteAddr)
		}
	}
}

func (cs *CallSession) Close() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	select {
	case <-cs.closed:
		return
	default:
		close(cs.closed)
	}
	if cs.conn != nil {
		cs.conn.Close()
	}
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
	default:
		d.inner = &rtp.G711Depacketizer{Codec: avframe.CodecG711U}
	}
	return d
}

func (d *audioDepacketizer) depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
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
	default:
		p.inner = &rtp.G711Packetizer{}
	}
	return p
}

func (p *audioPacketizer) packetize(frame *avframe.AVFrame) ([]*pionrtp.Packet, error) {
	return p.inner.Packetize(frame, 1400)
}
