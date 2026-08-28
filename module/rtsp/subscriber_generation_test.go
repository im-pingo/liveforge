package rtsp

import (
	"io"
	"net"
	"net/http"
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

func TestRTSPDescribeRejectsPublisherBeforeAtomicStartupIsReady(t *testing.T) {
	server := core.NewServer(config.Defaults())
	stream, err := server.StreamHub().GetOrCreate("live/rtsp-not-ready")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SetPublisher(&generationRTSPPublisher{
		id: "not-ready", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(server, nil, nil)
	session := NewRTSPSession("not-ready", stream.Key())
	req := &Request{Method: "DESCRIBE", URL: "rtsp://host/live/rtsp-not-ready", Headers: make(http.Header)}
	if response := handler.HandleDescribe(req, session); response.StatusCode == http.StatusOK {
		t.Fatal("DESCRIBE succeeded before the publisher startup snapshot was ready")
	}
}

func TestRTSPPlayRejectsReplacementAfterDescribeSnapshot(t *testing.T) {
	server := core.NewServer(config.Defaults())
	stream, err := server.StreamHub().GetOrCreate("live/rtsp-describe-generation")
	if err != nil {
		t.Fatal(err)
	}
	old := &generationRTSPPublisher{id: "old", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if err := stream.SetPublisher(old); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{1, 0x64},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		1000, 1000, []byte{0, 0, 0, 2, 0x65, 1},
	))

	handler := NewHandler(server, nil, nil)
	session := NewRTSPSession("describe-generation", stream.Key())
	describe := &Request{Method: "DESCRIBE", URL: "rtsp://host/live/rtsp-describe-generation", Headers: make(http.Header)}
	if response := handler.HandleDescribe(describe, session); response.StatusCode != http.StatusOK {
		t.Fatalf("DESCRIBE status = %d, want 200", response.StatusCode)
	}
	setup := &Request{Method: "SETUP", URL: describe.URL + "/trackID=0", Headers: make(http.Header)}
	setup.Headers.Set("Transport", "RTP/AVP/TCP;unicast;interleaved=0-1")
	if response := handler.HandleSetup(setup, session, "127.0.0.1:12345"); response.StatusCode != http.StatusOK {
		t.Fatalf("SETUP status = %d, want 200", response.StatusCode)
	}

	stream.RemovePublisher()
	replacement := &generationRTSPPublisher{id: "replacement", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	if err := stream.SetPublisher(replacement); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{1, 0x42},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		2000, 2000, []byte{0, 0, 0, 2, 0x65, 2},
	))

	play := &Request{Method: "PLAY", URL: describe.URL, Headers: make(http.Header)}
	if response := handler.HandlePlay(play, session, "127.0.0.1:12345"); response.StatusCode == http.StatusOK {
		t.Error("PLAY paired the old DESCRIBE snapshot with the replacement publisher")
	}
	conn := &generationRTSPConn{writes: make(chan []byte, 16)}
	done := make(chan struct{})
	go func() {
		(&Module{server: server}).runSubscriberLoop(conn, session)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stale RTSP playback loop did not terminate")
	}
	select {
	case data := <-conn.writes:
		t.Fatalf("stale RTSP playback emitted %d replacement bytes", len(data))
	default:
	}
}

func TestRTSPDescribeIsSafeDuringPublisherRemoval(t *testing.T) {
	for i := 0; i < 100; i++ {
		server := core.NewServer(config.Defaults())
		stream, err := server.StreamHub().GetOrCreate("live/rtsp-concurrent-remove")
		if err != nil {
			t.Fatal(err)
		}
		publisher := &generationRTSPPublisher{id: "source", info: &avframe.MediaInfo{
			VideoCodec:          avframe.CodecH264,
			VideoSequenceHeader: []byte{1, 0x64},
		}}
		if err := stream.SetPublisher(publisher); err != nil {
			t.Fatal(err)
		}
		handler := NewHandler(server, nil, nil)
		session := NewRTSPSession("concurrent-remove", stream.Key())
		req := &Request{Method: "DESCRIBE", URL: "rtsp://host/live/rtsp-concurrent-remove", Headers: make(http.Header)}
		removed := make(chan struct{})
		go func() {
			stream.RemovePublisherIf(publisher)
			close(removed)
		}()
		response := handler.HandleDescribe(req, session)
		<-removed
		if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNotFound {
			t.Fatalf("DESCRIBE status during removal = %d, want 200 or 404", response.StatusCode)
		}
	}
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
	if !session.SetDescription(stream.StartupSnapshot(), stream) {
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
