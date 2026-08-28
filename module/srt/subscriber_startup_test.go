package srt

import (
	"net"
	"testing"
	"time"

	gosrt "github.com/datarhei/gosrt"
	"github.com/datarhei/gosrt/packet"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

type startupSRTConn struct {
	writes chan []byte
}

func (c *startupSRTConn) Read([]byte) (int, error)           { return 0, net.ErrClosed }
func (c *startupSRTConn) ReadPacket() (packet.Packet, error) { return nil, net.ErrClosed }
func (c *startupSRTConn) WritePacket(packet.Packet) error    { return nil }
func (c *startupSRTConn) Close() error                       { return nil }
func (c *startupSRTConn) LocalAddr() net.Addr                { return &net.UDPAddr{} }
func (c *startupSRTConn) RemoteAddr() net.Addr               { return &net.UDPAddr{} }
func (c *startupSRTConn) SetDeadline(time.Time) error        { return nil }
func (c *startupSRTConn) SetReadDeadline(time.Time) error    { return nil }
func (c *startupSRTConn) SetWriteDeadline(time.Time) error   { return nil }
func (c *startupSRTConn) SocketId() uint32                   { return 1 }
func (c *startupSRTConn) PeerSocketId() uint32               { return 2 }
func (c *startupSRTConn) StreamId() string                   { return "subscribe:/live/startup" }
func (c *startupSRTConn) Stats(*gosrt.Statistics)            {}
func (c *startupSRTConn) Version() uint32                    { return 5 }
func (c *startupSRTConn) Write(p []byte) (int, error) {
	copyData := append([]byte(nil), p...)
	c.writes <- copyData
	return len(p), nil
}

type srtStartupPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *srtStartupPublisher) ID() string                    { return p.id }
func (p *srtStartupPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *srtStartupPublisher) Close() error                  { return nil }

func newSRTStartupSubscriber(t *testing.T, info *avframe.MediaInfo) (*core.Stream, *Subscriber, *startupSRTConn, chan struct{}) {
	t.Helper()
	bus := core.NewEventBus()
	hub := core.NewStreamHub(config.StreamConfig{GOPCache: true, GOPCacheNum: 1, RingBufferSize: 32}, config.LimitsConfig{}, bus)
	stream, err := hub.GetOrCreate("live/startup")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&srtStartupPublisher{id: "old", info: info}); err != nil {
		t.Fatal(err)
	}
	conn := &startupSRTConn{writes: make(chan []byte, 16)}
	sub := NewSubscriber(conn, "live/startup", hub, bus, nil)
	done := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-sub.closed:
		default:
			close(sub.closed)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	})
	return stream, sub, conn, done
}

func startSRTSubscriber(sub *Subscriber, done chan<- struct{}) {
	sub.Run()
	close(done)
}

func readSRTWrite(t *testing.T, conn *startupSRTConn) []byte {
	t.Helper()
	select {
	case data := <-conn.writes:
		return data
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SRT output")
		return nil
	}
}

func TestSRTStartupSnapshotDeliversCrossTrackLiveAudio(t *testing.T) {
	stream, sub, conn, done := newSRTStartupSubscriber(t, &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC})
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{1, 0x64}))
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader, 0, 0, []byte{0x12, 0x10}))
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 4000, 4000, []byte{0, 0, 0, 2, 0x65, 1}))
	go startSRTSubscriber(sub, done)
	readSRTWrite(t, conn)
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 1000, 1000, []byte{1, 2}))
	readSRTWrite(t, conn)
}

func TestSRTLateSequenceHeaderRefreshesTrackConfiguration(t *testing.T) {
	stream, sub, conn, done := newSRTStartupSubscriber(t, &avframe.MediaInfo{VideoCodec: avframe.CodecH264})
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{1, 0x64}))
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0, 0, 0, 2, 0x65, 1}))
	go startSRTSubscriber(sub, done)
	readSRTWrite(t, conn)
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader, 20, 20, []byte{0x12, 0x10}))
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 40, 40, []byte{1, 2}))
	announcement := readSRTWrite(t, conn)
	if !tsOutputDeclaresStreamType(announcement, 0x0f) {
		t.Fatal("SRT TS output did not announce the late AAC track before its first media frame")
	}
	media := readSRTWrite(t, conn)
	if !tsOutputContainsPID(media, ts.PIDAudio) {
		t.Fatal("SRT TS output dropped the first late-track audio frame")
	}
	select {
	case data := <-conn.writes:
		t.Fatalf("SRT TS output duplicated the first late-track audio frame in %d extra bytes", len(data))
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSRTSubscriberStopsBeforeReplacementGenerationFrame(t *testing.T) {
	stream, sub, conn, done := newSRTStartupSubscriber(t, &avframe.MediaInfo{VideoCodec: avframe.CodecH264})
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{1, 0x64}))
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 1000, 1000, []byte{0, 0, 0, 2, 0x65, 1}))
	go startSRTSubscriber(sub, done)
	readSRTWrite(t, conn)
	stream.RemovePublisher()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old SRT subscriber did not stop when its publisher generation ended")
	}
	if err := stream.SetPublisher(&srtStartupPublisher{id: "replacement", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{1, 0x42}))
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 2000, 2000, []byte{0, 0, 0, 2, 0x65, 2}))
	select {
	case data := <-conn.writes:
		t.Fatalf("old SRT subscriber received %d replacement bytes", len(data))
	case <-time.After(100 * time.Millisecond):
	}
}

func tsOutputDeclaresStreamType(data []byte, streamType byte) bool {
	for offset := 0; offset+ts.PacketSize <= len(data); offset += ts.PacketSize {
		pkt := data[offset : offset+ts.PacketSize]
		pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
		if pid != ts.PIDPmt || pkt[1]&0x40 == 0 {
			continue
		}
		pos := 4
		if pkt[3]&0x20 != 0 {
			pos += 1 + int(pkt[4])
		}
		if pos >= len(pkt) {
			continue
		}
		pos += 1 + int(pkt[pos])
		if pos+12 > len(pkt) {
			continue
		}
		sectionLen := int(pkt[pos+1]&0x0f)<<8 | int(pkt[pos+2])
		end := pos + 3 + sectionLen - 4
		programInfoLen := int(pkt[pos+10]&0x0f)<<8 | int(pkt[pos+11])
		for i := pos + 12 + programInfoLen; i+4 < end && i+4 < len(pkt); {
			if pkt[i] == streamType {
				return true
			}
			i += 5 + int(pkt[i+3]&0x0f)<<8 + int(pkt[i+4])
		}
	}
	return false
}

func tsOutputContainsPID(data []byte, wantPID uint16) bool {
	for offset := 0; offset+ts.PacketSize <= len(data); offset += ts.PacketSize {
		pkt := data[offset : offset+ts.PacketSize]
		pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
		if pid == wantPID {
			return true
		}
	}
	return false
}
