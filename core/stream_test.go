package core

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func newTestStreamConfig() config.StreamConfig {
	return config.StreamConfig{
		GOPCache:           true,
		GOPCacheNum:        1,
		AudioCacheMs:       1000,
		RingBufferSize:     256,
		IdleTimeout:        5 * time.Second,
		NoPublisherTimeout: 3 * time.Second,
	}
}

type testPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *testPublisher) ID() string                   { return p.id }
func (p *testPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *testPublisher) Close() error                  { return nil }

func TestStreamStateTransitions(t *testing.T) {
	bus := NewEventBus()
	s := NewStream("live/test", newTestStreamConfig(), config.LimitsConfig{}, bus)

	if s.State() != StreamStateIdle {
		t.Fatalf("expected idle, got %v", s.State())
	}

	pub := &testPublisher{
		id:   "pub1",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC},
	}
	err := s.SetPublisher(pub)
	if err != nil {
		t.Fatalf("SetPublisher error: %v", err)
	}
	if s.State() != StreamStatePublishing {
		t.Fatalf("expected publishing, got %v", s.State())
	}

	s.RemovePublisher()
	if s.State() != StreamStateNoPublisher {
		t.Fatalf("expected no_publisher, got %v", s.State())
	}
}

func TestStreamRejectDuplicatePublisher(t *testing.T) {
	bus := NewEventBus()
	s := NewStream("live/test", newTestStreamConfig(), config.LimitsConfig{}, bus)

	pub1 := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	pub2 := &testPublisher{id: "pub2", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}

	_ = s.SetPublisher(pub1)
	err := s.SetPublisher(pub2)
	if err == nil {
		t.Error("expected error for duplicate publisher")
	}
}

func TestStreamWriteAndReadFrames(t *testing.T) {
	bus := NewEventBus()
	s := NewStream("live/test", newTestStreamConfig(), config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC}}
	_ = s.SetPublisher(pub)

	// Write frames
	keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x65, 0x01})
	s.WriteFrame(keyframe)

	audio := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, []byte{0xFF, 0x01})
	s.WriteFrame(audio)

	inter := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{0x41, 0x01})
	s.WriteFrame(inter)

	// Verify GOP cache
	gop := s.GOPCache()
	if len(gop) < 1 {
		t.Fatal("expected at least 1 frame in GOP cache")
	}
	if gop[0].FrameType != avframe.FrameTypeKeyframe {
		t.Error("first frame in GOP should be keyframe")
	}
}

func TestStreamNoPublisherTimeout(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.NoPublisherTimeout = 100 * time.Millisecond // short timeout for test
	s := NewStream("live/timeout", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)
	s.RemovePublisher()

	if s.State() != StreamStateNoPublisher {
		t.Fatalf("expected no_publisher, got %v", s.State())
	}

	// Wait for timeout
	time.Sleep(200 * time.Millisecond)
	if s.State() != StreamStateDestroying {
		t.Errorf("expected destroying after timeout, got %v", s.State())
	}
}

func TestStreamGOPCacheDisabled(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = false
	s := NewStream("live/no-gop", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)

	keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x65})
	s.WriteFrame(keyframe)

	inter := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{0x41})
	s.WriteFrame(inter)

	gop := s.GOPCache()
	if len(gop) != 0 {
		t.Errorf("expected empty GOP cache when disabled, got %d frames", len(gop))
	}
}

func TestStreamMultiGOPCache(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = true
	cfg.GOPCacheNum = 3
	s := NewStream("live/multi-gop", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)

	// Write 4 GOPs: each has keyframe + 2 interframes
	dtsValues := [][]int64{
		{0, 40, 80},
		{120, 160, 200},
		{240, 280, 320},
		{360, 400, 440},
	}
	for _, gopDTS := range dtsValues {
		kf := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, gopDTS[0], gopDTS[0], []byte{0x65})
		s.WriteFrame(kf)
		for _, dts := range gopDTS[1:] {
			inter := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, dts, dts, []byte{0x41})
			s.WriteFrame(inter)
		}
	}

	// With GOPCacheNum=3, should retain GOPs 2,3,4 (9 frames), first frame DTS=120
	gop := s.GOPCache()
	if len(gop) != 9 {
		t.Errorf("expected 9 cached frames, got %d", len(gop))
	}
	if len(gop) > 0 && gop[0].DTS != 120 {
		t.Errorf("expected first frame DTS=120, got %d", gop[0].DTS)
	}
	if s.GOPCacheLen() != 9 {
		t.Errorf("expected GOPCacheLen=9, got %d", s.GOPCacheLen())
	}
}

func TestStreamAudioCacheMs(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = false
	cfg.AudioCacheMs = 500
	s := NewStream("live/audio-cache", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{AudioCodec: avframe.CodecAAC}}
	_ = s.SetPublisher(pub)

	// Write 10 audio frames at 100ms DTS intervals (0, 100, 200, ..., 900)
	for i := 0; i < 10; i++ {
		af := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, int64(i*100), int64(i*100), []byte{0xFF})
		s.WriteFrame(af)
	}

	// With AudioCacheMs=500, last frame DTS=900, minDTS=400
	// Frames with DTS >= 400 remain: 400, 500, 600, 700, 800, 900 = 6 frames
	ac := s.AudioCache()
	if len(ac) != 6 {
		t.Errorf("expected 6 audio cache frames, got %d", len(ac))
	}
	if len(ac) > 0 && ac[0].DTS != 400 {
		t.Errorf("expected first audio frame DTS=400, got %d", ac[0].DTS)
	}
}

func TestStreamIdleTimeout(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.IdleTimeout = 100 * time.Millisecond
	cfg.NoPublisherTimeout = 0 // disable so it doesn't interfere
	s := NewStream("live/idle", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)
	_ = s.AddSubscriber("rtmp")

	// Remove both pub and sub
	s.RemovePublisher()
	s.RemoveSubscriber("rtmp")

	// Wait for idle timeout
	time.Sleep(200 * time.Millisecond)
	if s.State() != StreamStateDestroying {
		t.Errorf("expected destroying after idle timeout, got %v", s.State())
	}
}

func TestStreamIdleTimeoutCancelledBySubscriber(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.IdleTimeout = 200 * time.Millisecond
	cfg.NoPublisherTimeout = 0
	s := NewStream("live/idle-cancel", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)
	s.RemovePublisher()

	// Idle timer should start (no pub, no sub)
	// Add subscriber before timeout expires
	time.Sleep(50 * time.Millisecond)
	_ = s.AddSubscriber("rtmp")

	// Wait past the original timeout
	time.Sleep(200 * time.Millisecond)
	if s.State() == StreamStateDestroying {
		t.Error("stream should not be destroying — subscriber cancelled idle timeout")
	}
}

func TestStreamAudioCacheDisabled(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = false
	cfg.AudioCacheMs = 0
	s := NewStream("live/audio-disabled", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{AudioCodec: avframe.CodecAAC}}
	_ = s.SetPublisher(pub)

	af := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 100, 100, []byte{0xFF})
	s.WriteFrame(af)

	ac := s.AudioCache()
	if len(ac) != 0 {
		t.Errorf("expected empty audio cache when disabled, got %d frames", len(ac))
	}
}

func TestSkipTracker(t *testing.T) {
	st := NewSkipTracker(3, 10*time.Second)

	// First 3 skips should be OK (not exceed threshold)
	for i := 0; i < 3; i++ {
		if st.RecordSkip() {
			t.Errorf("skip %d should not exceed threshold", i+1)
		}
	}

	// 4th skip should exceed threshold
	if !st.RecordSkip() {
		t.Error("4th skip should exceed threshold")
	}
}

func TestSkipTrackerWindowExpiry(t *testing.T) {
	st := NewSkipTracker(3, 100*time.Millisecond)

	// Record 2 skips
	st.RecordSkip()
	st.RecordSkip()

	// Wait for window to expire
	time.Sleep(150 * time.Millisecond)

	// 3rd skip should be OK (previous ones expired)
	if st.RecordSkip() {
		t.Error("skip after window expiry should not exceed threshold")
	}
}

func TestSkipTrackerDisabled(t *testing.T) {
	st := NewSkipTracker(0, 10*time.Second)
	if st.RecordSkip() {
		t.Error("disabled tracker should never exceed threshold")
	}
}

func TestStreamMaxBitrateEnforcement(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = false
	limits := config.LimitsConfig{MaxBitratePerStream: 1} // 1 kbps — very low
	s := NewStream("live/bitrate-test", cfg, limits, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)

	// Sequence headers must always be accepted regardless of bitrate
	seqHeader := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, make([]byte, 10000))
	if ok := s.WriteFrame(seqHeader); !ok {
		t.Error("sequence header should always be accepted even when over bitrate")
	}

	// Write a large frame to push bitrate over 1 kbps
	bigFrame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, make([]byte, 100000))
	s.WriteFrame(bigFrame)

	// Wait so that elapsed time > 0ms for bitrate computation
	time.Sleep(2 * time.Millisecond)

	// After writing 100KB, BitrateKbps >> 1. Next frame should be rejected.
	nextFrame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, make([]byte, 1000))
	if ok := s.WriteFrame(nextFrame); ok {
		t.Error("frame should be rejected when bitrate exceeds max_bitrate_per_stream")
	}
}

func TestStreamMaxBitrateDisabled(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = false
	limits := config.LimitsConfig{MaxBitratePerStream: 0} // disabled
	s := NewStream("live/bitrate-disabled", cfg, limits, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)

	bigFrame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, make([]byte, 100000))
	if ok := s.WriteFrame(bigFrame); !ok {
		t.Error("frame should be accepted when max_bitrate_per_stream is disabled")
	}
}

func TestStreamStateString(t *testing.T) {
	tests := []struct {
		state StreamState
		want  string
	}{
		{StreamStateIdle, "idle"},
		{StreamStateWaitingPull, "waiting_pull"},
		{StreamStatePublishing, "publishing"},
		{StreamStateNoPublisher, "no_publisher"},
		{StreamStateDestroying, "destroying"},
		{StreamState(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.want {
			t.Errorf("StreamState(%d).String() = %q, want %q", tt.state, got, tt.want)
		}
	}
}

func TestStreamAccessors(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	s := NewStream("live/accessors", cfg, config.LimitsConfig{}, bus)

	if s.Key() != "live/accessors" {
		t.Errorf("expected key live/accessors, got %s", s.Key())
	}
	if s.Config().RingBufferSize != cfg.RingBufferSize {
		t.Error("Config() should return matching config")
	}
	if s.RingBuffer() == nil {
		t.Error("RingBuffer() should not be nil")
	}
	if s.MuxerManager() == nil {
		t.Error("MuxerManager() should not be nil")
	}
	if s.FeedbackRouter() == nil {
		t.Error("FeedbackRouter() should not be nil")
	}
	if s.Publisher() != nil {
		t.Error("Publisher() should be nil initially")
	}
	if s.VideoSeqHeader() != nil {
		t.Error("VideoSeqHeader() should be nil initially")
	}
	if s.AudioSeqHeader() != nil {
		t.Error("AudioSeqHeader() should be nil initially")
	}

	stats := s.Stats()
	if stats.BytesIn != 0 {
		t.Error("initial BytesIn should be 0")
	}
}

func TestStreamClose(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	s := NewStream("live/close", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub)

	s.Close()
	if s.State() != StreamStateDestroying {
		t.Errorf("expected destroying after Close, got %v", s.State())
	}

	// Double close should not panic
	s.Close()
}

func TestStreamSubscribers(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	s := NewStream("live/subs", cfg, config.LimitsConfig{}, bus)

	_ = s.AddSubscriber("rtmp")
	_ = s.AddSubscriber("rtmp")
	_ = s.AddSubscriber("hls")

	subs := s.Subscribers()
	if subs["rtmp"] != 2 {
		t.Errorf("expected 2 rtmp subscribers, got %d", subs["rtmp"])
	}
	if subs["hls"] != 1 {
		t.Errorf("expected 1 hls subscriber, got %d", subs["hls"])
	}

	s.RemoveSubscriber("rtmp")
	subs = s.Subscribers()
	if subs["rtmp"] != 1 {
		t.Errorf("expected 1 rtmp subscriber after remove, got %d", subs["rtmp"])
	}

	s.RemoveSubscriber("rtmp")
	subs = s.Subscribers()
	if _, ok := subs["rtmp"]; ok {
		t.Error("rtmp should be removed from subscribers map when count reaches 0")
	}
}

func TestStreamMaxSubscribers(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	limits := config.LimitsConfig{MaxSubscribersPerStream: 2}
	s := NewStream("live/max-subs", cfg, limits, bus)

	if err := s.AddSubscriber("rtmp"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSubscriber("hls"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSubscriber("webrtc"); err == nil {
		t.Error("expected error when exceeding max_subscribers_per_stream")
	}
}

func TestStreamSeqHeaderCaching(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = false
	s := NewStream("live/seqheader", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC}}
	_ = s.SetPublisher(pub)

	vsh := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{0x67, 0x42})
	s.WriteFrame(vsh)

	ash := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader, 0, 0, []byte{0x12, 0x10})
	s.WriteFrame(ash)

	if s.VideoSeqHeader() == nil {
		t.Error("VideoSeqHeader should be cached")
	}
	if s.AudioSeqHeader() == nil {
		t.Error("AudioSeqHeader should be cached")
	}
}

func TestStreamGOPCacheDetail(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.GOPCache = true
	cfg.GOPCacheNum = 1
	s := NewStream("live/gop-detail", cfg, config.LimitsConfig{}, bus)

	pub := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecAAC}}
	_ = s.SetPublisher(pub)

	// Empty GOP cache detail
	d := s.GOPCacheDetail()
	if d.TotalFrames != 0 {
		t.Errorf("expected 0 total frames, got %d", d.TotalFrames)
	}

	// Write keyframe + interframe + audio
	kf := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 100, 100, []byte{0x65})
	s.WriteFrame(kf)
	inter := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 140, 140, []byte{0x41})
	s.WriteFrame(inter)
	af := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 120, 120, []byte{0xFF})
	s.WriteFrame(af)

	d = s.GOPCacheDetail()
	if d.TotalFrames != 3 {
		t.Errorf("expected 3 total frames, got %d", d.TotalFrames)
	}
	if d.VideoFrames != 2 {
		t.Errorf("expected 2 video frames, got %d", d.VideoFrames)
	}
	if d.AudioFrames != 1 {
		t.Errorf("expected 1 audio frame, got %d", d.AudioFrames)
	}
	// GOP order: keyframe(100), interframe(140), audio(120)
	// DurationMs = lastDTS - firstDTS = 120 - 100 = 20 (audio is last in sequence)
	if d.DurationMs != 20 {
		t.Errorf("expected duration 20ms, got %d", d.DurationMs)
	}
}

func TestStreamRepublishBeforeTimeout(t *testing.T) {
	bus := NewEventBus()
	cfg := newTestStreamConfig()
	cfg.NoPublisherTimeout = 500 * time.Millisecond
	s := NewStream("live/republish", cfg, config.LimitsConfig{}, bus)

	pub1 := &testPublisher{id: "pub1", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	_ = s.SetPublisher(pub1)
	s.RemovePublisher()

	// Republish before timeout with same codec
	pub2 := &testPublisher{id: "pub2", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}
	err := s.SetPublisher(pub2)
	if err != nil {
		t.Fatalf("republish should succeed: %v", err)
	}
	if s.State() != StreamStatePublishing {
		t.Errorf("expected publishing after republish, got %v", s.State())
	}
}
