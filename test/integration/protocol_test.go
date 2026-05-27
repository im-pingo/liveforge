package integration

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	rtspmod "github.com/im-pingo/liveforge/module/rtsp"
	srtmod "github.com/im-pingo/liveforge/module/srt"
)

func TestRTSPServerStartStop(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Name = "rtsp-integration"
	cfg.RTSP.Enabled = true
	cfg.RTSP.Listen = "127.0.0.1:0"
	cfg.RTSP.RTPPortRange = []int{30000, 30100}
	cfg.Stream.GOPCache = true
	cfg.Stream.GOPCacheNum = 1
	cfg.Stream.RingBufferSize = 256
	cfg.Stream.NoPublisherTimeout = 5 * time.Second

	s := core.NewServer(cfg)
	s.RegisterModule(rtspmod.NewModule())

	if err := s.Init(); err != nil {
		t.Fatalf("server init: %v", err)
	}

	s.Shutdown()
}

func TestSRTServerStartStop(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Name = "srt-integration"
	cfg.SRT.Enabled = true
	cfg.SRT.Listen = "127.0.0.1:0"
	cfg.SRT.Latency = 120
	cfg.Stream.GOPCache = true
	cfg.Stream.GOPCacheNum = 1
	cfg.Stream.RingBufferSize = 256
	cfg.Stream.NoPublisherTimeout = 5 * time.Second

	s := core.NewServer(cfg)
	s.RegisterModule(srtmod.NewModule())

	if err := s.Init(); err != nil {
		t.Fatalf("server init: %v", err)
	}

	s.Shutdown()
}

func TestMultiProtocolPublishSubscribe(t *testing.T) {
	bus := core.NewEventBus()
	cfg := config.StreamConfig{
		GOPCache:           true,
		GOPCacheNum:        2,
		AudioCacheMs:       1000,
		RingBufferSize:     512,
		NoPublisherTimeout: 5 * time.Second,
	}

	hub := core.NewStreamHub(cfg, config.LimitsConfig{}, bus)

	stream, err := hub.GetOrCreate("live/multiproto-test")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	pub := &testPublisher{
		id: "rtsp-pub",
		info: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecAAC,
		},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatalf("SetPublisher: %v", err)
	}

	// Write sequence headers
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64, 0x00, 0x28},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x12, 0x10},
	))

	// Write two GOPs
	for gop := 0; gop < 2; gop++ {
		baseTS := int64(gop * 2000)
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
			baseTS, baseTS, []byte{0x65, 0x88, byte(gop)},
		))
		for i := 1; i <= 4; i++ {
			ts := baseTS + int64(i*40)
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
				ts, ts, []byte{0x41, byte(i)},
			))
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
				ts, ts, []byte{0xFF, byte(i)},
			))
		}
	}

	// Verify GOP cache has 2 GOPs
	gop := stream.GOPCache()
	keyframeCount := 0
	for _, f := range gop {
		if f.FrameType == avframe.FrameTypeKeyframe {
			keyframeCount++
		}
	}
	if keyframeCount != 2 {
		t.Errorf("expected 2 keyframes in GOP cache, got %d", keyframeCount)
	}

	// Multiple concurrent readers
	readers := make([]*avframe.AVFrame, 0)
	reader := stream.RingBuffer().NewReader()
	for {
		frame, ok := reader.TryRead()
		if !ok {
			break
		}
		readers = append(readers, frame)
	}

	// 2 seq headers + 2 GOPs * (1 keyframe + 4 inter + 4 audio) = 2 + 18 = 20
	if len(readers) != 20 {
		t.Errorf("expected 20 frames, got %d", len(readers))
	}

	stream.RemovePublisher()
	hub.Remove("live/multiproto-test")
}

func TestHLSMuxerLifecycle(t *testing.T) {
	bus := core.NewEventBus()
	cfg := config.StreamConfig{
		GOPCache:           true,
		GOPCacheNum:        1,
		RingBufferSize:     256,
		NoPublisherTimeout: 5 * time.Second,
	}

	hub := core.NewStreamHub(cfg, config.LimitsConfig{}, bus)

	stream, err := hub.GetOrCreate("live/hls-test")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	pub := &testPublisher{
		id:   "hls-pub",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatalf("SetPublisher: %v", err)
	}

	mm := stream.MuxerManager()

	// Register a test muxer to verify lifecycle
	started := make(chan struct{}, 1)
	mm.RegisterMuxerStart("flv", func(inst *core.MuxerInstance, s *core.Stream) {
		started <- struct{}{}
	})

	// Request the muxer
	reader, inst := mm.GetOrCreateMuxer("flv")
	if inst == nil {
		t.Fatal("expected muxer instance")
	}
	_ = reader

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("muxer start callback not called within 1s")
	}

	stream.RemovePublisher()
	hub.Remove("live/hls-test")
}

func TestStreamStatsIntegration(t *testing.T) {
	bus := core.NewEventBus()
	cfg := config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 256,
		NoPublisherTimeout: 5 * time.Second,
	}

	hub := core.NewStreamHub(cfg, config.LimitsConfig{}, bus)

	stream, err := hub.GetOrCreate("live/stats-test")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}

	pub := &testPublisher{
		id:   "stats-pub",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatalf("SetPublisher: %v", err)
	}

	// Write frames and verify stats
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64, 0x00, 0x28},
	))

	payload := make([]byte, 1000)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, payload,
	))

	stats := stream.Stats()
	if stats.BytesIn == 0 {
		t.Error("expected non-zero bytes in")
	}
	if stats.VideoFrames == 0 {
		t.Error("expected non-zero video frames")
	}

	stream.RemovePublisher()
	hub.Remove("live/stats-test")
}
