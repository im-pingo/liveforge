package rtmp

import (
	"net"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

type startupPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *startupPublisher) ID() string                    { return p.id }
func (p *startupPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *startupPublisher) Close() error                  { return nil }

func newRTMPStartupStream(t *testing.T, info *avframe.MediaInfo) *core.Stream {
	t.Helper()
	stream := core.NewStream("live/startup", config.StreamConfig{
		GOPCache: true, GOPCacheNum: 1, RingBufferSize: 32,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&startupPublisher{id: "old", info: info}); err != nil {
		t.Fatal(err)
	}
	return stream
}

func startRTMPStartupSubscriber(t *testing.T, stream *core.Stream) (*Subscriber, net.Conn, *ChunkReader, <-chan struct{}) {
	t.Helper()
	client, server := net.Pipe()
	sub := NewSubscriber("live/startup", server, NewChunkWriter(server, DefaultChunkSize), stream, nil)
	done := make(chan struct{})
	go func() {
		sub.WriteLoop()
		close(done)
	}()
	t.Cleanup(func() {
		_ = client.Close()
		_ = sub.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	})
	return sub, client, NewChunkReader(client, DefaultChunkSize), done
}

func readRTMPStartupMessage(t *testing.T, conn net.Conn, reader *ChunkReader) *Message {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	msg, err := reader.ReadMessage()
	if err != nil {
		t.Fatalf("read RTMP message: %v", err)
	}
	return msg
}

func TestRTMPStartupSnapshotDeliversCrossTrackLiveAudio(t *testing.T) {
	stream := newRTMPStartupStream(t, &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC})
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{1, 0x64}))
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader, 0, 0, []byte{0x12, 0x10}))
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 4000, 4000, []byte{0, 0, 0, 2, 0x65, 1}))

	_, conn, reader, _ := startRTMPStartupSubscriber(t, stream)
	for range 3 {
		readRTMPStartupMessage(t, conn, reader)
	}
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 1000, 1000, []byte{1, 2}))
	msg := readRTMPStartupMessage(t, conn, reader)
	if msg.TypeID != MsgAudio || msg.Timestamp != 1000 {
		t.Fatalf("first live message = type %d DTS %d, want audio DTS 1000", msg.TypeID, msg.Timestamp)
	}
}

func TestRTMPLateSequenceHeaderReachesSubscriber(t *testing.T) {
	stream := newRTMPStartupStream(t, &avframe.MediaInfo{VideoCodec: avframe.CodecH264})
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{1, 0x64}))
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 4000, 4000, []byte{0, 0, 0, 2, 0x65, 1}))

	_, conn, reader, _ := startRTMPStartupSubscriber(t, stream)
	for range 2 {
		readRTMPStartupMessage(t, conn, reader)
	}
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader, 4100, 4100, []byte{0x12, 0x10}))
	msg := readRTMPStartupMessage(t, conn, reader)
	if msg.TypeID != MsgAudio || msg.Timestamp != 4100 {
		t.Fatalf("late sequence header = type %d DTS %d, want audio DTS 4100", msg.TypeID, msg.Timestamp)
	}
}

func TestRTMPSubscriberStopsBeforeReplacementGenerationFrame(t *testing.T) {
	stream := newRTMPStartupStream(t, &avframe.MediaInfo{VideoCodec: avframe.CodecH264})
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{1, 0x64}))
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 1000, 1000, []byte{0, 0, 0, 2, 0x65, 1}))

	_, conn, reader, done := startRTMPStartupSubscriber(t, stream)
	for range 2 {
		readRTMPStartupMessage(t, conn, reader)
	}
	stream.RemovePublisher()
	if err := stream.SetPublisher(&startupPublisher{id: "replacement", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{1, 0x42}))
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 2000, 2000, []byte{0, 0, 0, 2, 0x65, 2}))

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old RTMP subscriber did not stop when its publisher generation ended")
	}
	_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if msg, err := reader.ReadMessage(); err == nil {
		t.Fatalf("old RTMP subscriber received replacement message type %d DTS %d", msg.TypeID, msg.Timestamp)
	}
}
