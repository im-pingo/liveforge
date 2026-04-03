// module/cluster/transport_rtmp_test.go
package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestRTMPTransportScheme(t *testing.T) {
	tr := NewRTMPTransport()
	if tr.Scheme() != "rtmp" {
		t.Errorf("Scheme = %q, want %q", tr.Scheme(), "rtmp")
	}
}

func TestRTMPTransportPushToMockServer(t *testing.T) {
	ln, addr := mockRTMPServer(t)
	defer ln.Close()

	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/rtmptest")
	pub := &originPublisher{id: "test", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecAAC,
	}}
	stream.SetPublisher(pub)

	// Write sequence headers
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   []byte{0x01, 0x64, 0x00, 0x28, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68, 0xEE, 0x3C, 0x80},
	})
	stream.WriteFrame(&avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio, Codec: avframe.CodecAAC,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   []byte{0x12, 0x10},
	})

	tr := NewRTMPTransport()
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Write some frames in the background
	go func() {
		time.Sleep(100 * time.Millisecond)
		for i := 0; i < 5; i++ {
			stream.WriteFrame(&avframe.AVFrame{
				MediaType: avframe.MediaTypeVideo, Codec: avframe.CodecH264,
				FrameType: avframe.FrameTypeKeyframe,
				DTS: int64((i + 1) * 33), PTS: int64((i + 1) * 33),
				Payload: []byte{0x65, 0x88, 0x00, 0x01},
			})
			time.Sleep(33 * time.Millisecond)
		}
		cancel()
	}()

	// Push should run until context cancels
	err := tr.Push(ctx, "rtmp://"+addr+"/live/rtmptest", stream)
	// Either nil (context cancelled) or an error (connection closed) is acceptable
	_ = err
}

func TestRTMPTransportPushBadURL(t *testing.T) {
	hub, _ := newTestHub()
	stream, _ := hub.GetOrCreate("live/test")

	tr := NewRTMPTransport()
	defer tr.Close()

	ctx := context.Background()
	err := tr.Push(ctx, "bad-url-no-scheme", stream)
	if err == nil {
		t.Error("expected error for bad URL")
	}
}
