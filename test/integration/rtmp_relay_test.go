package integration

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	rtmpmod "github.com/im-pingo/liveforge/module/rtmp"
)

type testPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *testPublisher) ID() string                   { return p.id }
func (p *testPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *testPublisher) Close() error                  { return nil }

func TestRTMPServerStartStop(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Name = "integration-test"
	cfg.RTMP.Enabled = true
	cfg.RTMP.Listen = "127.0.0.1:0"
	cfg.RTMP.ChunkSize = 4096
	cfg.Stream.GOPCache = true
	cfg.Stream.GOPCacheNum = 1
	cfg.Stream.RingBufferSize = 256
	cfg.Stream.NoPublisherTimeout = 5 * time.Second

	s := core.NewServer(cfg)
	mod := rtmpmod.NewModule()
	s.RegisterModule(mod)

	if err := s.Init(); err != nil {
		t.Fatalf("server init: %v", err)
	}

	// Verify hub exists
	if mod.Hub() == nil {
		t.Fatal("expected hub to be initialized")
	}

	s.Shutdown()
	t.Log("RTMP server start/stop integration test passed")
}

func TestPublishSubscribeFlow(t *testing.T) {
	// Test the full publish → subscribe data flow through core components
	bus := core.NewEventBus()
	cfg := config.StreamConfig{
		GOPCache:           true,
		GOPCacheNum:        1,
		AudioCacheMs:       1000,
		RingBufferSize:     256,
		NoPublisherTimeout: 5 * time.Second,
	}

	hub := core.NewStreamHub(cfg, config.LimitsConfig{}, bus)

	// Create stream via hub
	stream, err := hub.GetOrCreate("live/integration-test")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if stream.State() != core.StreamStateIdle {
		t.Fatalf("expected idle, got %v", stream.State())
	}

	// Set publisher
	pub := &testPublisher{
		id:   "test-pub",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatalf("set publisher: %v", err)
	}
	if stream.State() != core.StreamStatePublishing {
		t.Fatalf("expected publishing, got %v", stream.State())
	}

	// Write sequence headers
	videoSeqHeader := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{0x01, 0x64, 0x00, 0x28})
	stream.WriteFrame(videoSeqHeader)

	audioSeqHeader := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader, 0, 0, []byte{0x12, 0x10})
	stream.WriteFrame(audioSeqHeader)

	// Write a GOP
	keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x65, 0x88, 0x00, 0x01})
	stream.WriteFrame(keyframe)

	audio1 := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 23, 23, []byte{0xFF, 0x01})
	stream.WriteFrame(audio1)

	inter1 := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{0x41, 0x01})
	stream.WriteFrame(inter1)

	audio2 := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 46, 46, []byte{0xFF, 0x02})
	stream.WriteFrame(audio2)

	inter2 := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 80, 80, []byte{0x41, 0x02})
	stream.WriteFrame(inter2)

	// Verify GOP cache
	gop := stream.GOPCache()
	if len(gop) < 3 {
		t.Fatalf("expected at least 3 frames in GOP, got %d", len(gop))
	}
	if gop[0].FrameType != avframe.FrameTypeKeyframe {
		t.Error("first GOP frame should be keyframe")
	}

	// Verify ring buffer contains all frames
	reader := stream.RingBuffer().NewReader()
	frameCount := 0
	for {
		_, ok := reader.TryRead()
		if !ok {
			break
		}
		frameCount++
	}
	if frameCount != 7 { // 2 seq headers + 5 media frames
		t.Errorf("expected 7 frames in ring buffer, got %d", frameCount)
	}

	// Verify hub has the stream
	if hub.Count() != 1 {
		t.Errorf("expected 1 stream in hub, got %d", hub.Count())
	}

	// Remove publisher
	stream.RemovePublisher()
	if stream.State() != core.StreamStateNoPublisher {
		t.Errorf("expected no_publisher, got %v", stream.State())
	}

	// Remove from hub
	hub.Remove("live/integration-test")
	if hub.Count() != 0 {
		t.Errorf("expected 0 streams, got %d", hub.Count())
	}

	t.Log("Full publish/subscribe flow integration test passed")
}
