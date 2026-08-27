package rtsp

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

type generationRTSPPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *generationRTSPPublisher) ID() string                    { return p.id }
func (p *generationRTSPPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *generationRTSPPublisher) Close() error                  { return nil }

type generationRTSPConn struct {
	writes chan []byte
}

func (c *generationRTSPConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *generationRTSPConn) Close() error                     { return nil }
func (c *generationRTSPConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *generationRTSPConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *generationRTSPConn) SetDeadline(time.Time) error      { return nil }
func (c *generationRTSPConn) SetReadDeadline(time.Time) error  { return nil }
func (c *generationRTSPConn) SetWriteDeadline(time.Time) error { return nil }
func (c *generationRTSPConn) Write(data []byte) (int, error) {
	c.writes <- append([]byte(nil), data...)
	return len(data), nil
}

func TestRTSPSubscriberStopsBeforeReplacementGenerationFrame(t *testing.T) {
	stream := core.NewStream("live/rtsp-generation", config.StreamConfig{
		GOPCache: true, GOPCacheNum: 1, RingBufferSize: 32,
	}, config.LimitsConfig{}, core.NewEventBus())
	info := &avframe.MediaInfo{VideoCodec: avframe.CodecH264}
	if err := stream.SetPublisher(&generationRTSPPublisher{id: "old", info: info}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{1, 0x64}))
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 1000, 1000, []byte{0, 0, 0, 2, 0x65, 1}))

	cfg := &config.Config{}
	cfg.Stream.RingBufferSize = 32
	module := &Module{server: core.NewServer(cfg)}
	session := NewRTSPSession("subscriber", "live/rtsp-generation")
	if !session.SetDescription(info, stream) {
		t.Fatal("set RTSP session description")
	}
	conn := &generationRTSPConn{writes: make(chan []byte, 16)}
	done := make(chan struct{})
	go func() {
		module.runSubscriberLoop(conn, session)
		close(done)
	}()

	select {
	case <-conn.writes:
	case <-time.After(time.Second):
		t.Fatal("old RTSP subscriber did not send its cached keyframe")
	}
	for {
		select {
		case <-conn.writes:
		default:
			goto drained
		}
	}

drained:
	stream.RemovePublisher()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old RTSP subscriber did not stop when its publisher generation ended")
	}
	if err := stream.SetPublisher(&generationRTSPPublisher{id: "replacement", info: info}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{1, 0x42}))
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 2000, 2000, []byte{0, 0, 0, 2, 0x65, 2}))
	select {
	case data := <-conn.writes:
		t.Fatalf("old RTSP subscriber received %d replacement bytes", len(data))
	case <-time.After(100 * time.Millisecond):
	}
}
