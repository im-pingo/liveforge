package webrtc

import (
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
)

func TestWHEPStartupSnapshotKeepsFramesWrittenWhileCacheIsSent(t *testing.T) {
	stream := core.NewStream("live/whep-snapshot", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 16,
	}, config.LimitsConfig{}, core.NewEventBus())
	cached := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeKeyframe, 1000, 1000, []byte{1})
	stream.WriteFrame(cached)

	snapshot := stream.StartupSnapshot()
	gopCache := whepLiveSnapshot(snapshot, false)
	if len(gopCache) != 1 || gopCache[0] != cached {
		t.Fatalf("GOP snapshot = %v, want cached keyframe", gopCache)
	}

	live := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe, 1033, 1033, []byte{2})
	stream.WriteFrame(live) // Simulates a publisher write while the cached GOP is sent.
	got, ok := stream.RingBuffer().NewReaderAt(snapshot.LiveCursor).TryRead()
	if !ok || got != live {
		t.Fatalf("first live frame after GOP snapshot = (%v, %v), want newly written frame", got, ok)
	}
}

func TestWHEPStartupSnapshotDropsSourceAudioWhenTranscoding(t *testing.T) {
	stream := core.NewStream("live/whep-transcode-cache", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 16,
	}, config.LimitsConfig{}, core.NewEventBus())
	video := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeKeyframe, 1000, 1000, []byte{1})
	aac := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 1020, 1020, []byte{2})
	stream.WriteFrame(video)
	stream.WriteFrame(aac)

	gopCache := whepLiveSnapshot(stream.StartupSnapshot(), true)
	if len(gopCache) != 1 || gopCache[0] != video {
		t.Fatalf("transcoded live GOP snapshot = %v, want cached video only", gopCache)
	}
}

func TestWHEPFeedReadersKeepAtomicSourceCursorWhenTranscoderUnavailable(t *testing.T) {
	stream := core.NewStream("live/whep-reader-transition", config.StreamConfig{
		RingBufferSize: 16,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{
		id:   "aac-publisher",
		info: &avframe.MediaInfo{VideoCodec: avframe.CodecH265, AudioCodec: avframe.CodecAAC},
	}); err != nil {
		t.Fatal(err)
	}
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, &audiocodec.Registry{}, 16))

	snapshot := stream.StartupSnapshot()
	betweenSnapshotAndReader := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe,
		1033, 1033, []byte{1},
	)
	stream.WriteFrame(betweenSnapshotAndReader)

	readers := newWHEPFeedReaders(stream, snapshot, true, avframe.CodecOpus)
	defer readers.Close()
	if got, ok := readers.source.TryRead(); !ok || got != betweenSnapshotAndReader {
		t.Fatalf("source reader first frame = (%v, %v), want frame written after snapshot", got, ok)
	}
	select {
	case <-readers.source.Signal(): // Consume the signal for the frame read above.
	default:
	}
	if readers.targetAudio == nil {
		t.Fatal("transcode reader missing")
	}

	if _, ok := readers.targetAudio.TryRead(); ok {
		t.Fatal("unavailable transcoder unexpectedly produced a frame")
	}

	woke := make(chan bool, 1)
	waitDone := make(chan struct{})
	go func() {
		woke <- readers.wait(waitDone, snapshot.GenerationDone)
	}()
	select {
	case <-woke:
		t.Fatal("reader wait returned before source video arrived")
	case <-time.After(20 * time.Millisecond):
	}

	afterUnavailableEpoch := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe,
		1066, 1066, []byte{2},
	)
	stream.WriteFrame(afterUnavailableEpoch)
	select {
	case ok := <-woke:
		if !ok {
			t.Fatal("reader wait stopped while the transcode epoch was unavailable")
		}
	case <-time.After(time.Second):
		close(waitDone)
		t.Fatal("source video did not wake reader while the transcode epoch was unavailable")
	}
	if got, ok := readers.source.TryRead(); !ok || got != afterUnavailableEpoch {
		t.Fatalf("source reader during unavailable transcode epoch = (%v, %v), want uninterrupted video", got, ok)
	}
}

func TestWHEPFeedReadersWakeIndependently(t *testing.T) {
	stream := core.NewStream("live/whep-independent-wake", config.StreamConfig{
		RingBufferSize: 16,
	}, config.LimitsConfig{}, core.NewEventBus())
	snapshot := stream.StartupSnapshot()
	r1 := newWHEPFeedReaders(stream, snapshot, false, 0)
	r2 := newWHEPFeedReaders(stream, snapshot, false, 0)
	defer r1.Close()
	defer r2.Close()

	done := make(chan struct{})
	woke1 := make(chan bool, 1)
	woke2 := make(chan bool, 1)
	go func() { woke1 <- r1.wait(done, snapshot.GenerationDone) }()
	go func() { woke2 <- r2.wait(done, snapshot.GenerationDone) }()
	time.Sleep(20 * time.Millisecond)
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 1, 1, []byte{1}))

	for i, woke := range []<-chan bool{woke1, woke2} {
		select {
		case ok := <-woke:
			if !ok {
				t.Fatalf("reader %d wait returned false after write", i+1)
			}
		case <-time.After(time.Second):
			t.Fatalf("reader %d did not wake after one source write", i+1)
		}
	}
	close(done)
}

func TestWHEPFeedReadersStopOnGenerationEnd(t *testing.T) {
	stream := core.NewStream("live/whep-generation", config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{
		id: "source", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264},
	}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader, 0, 0, []byte{1}))
	snapshot := stream.StartupSnapshot()
	readers := newWHEPFeedReaders(stream, snapshot, false, 0)
	defer readers.Close()
	done := make(chan struct{})
	stream.RemovePublisher()
	if readers.wait(done, snapshot.GenerationDone) {
		t.Fatal("WHEP reader remained active after publisher generation ended")
	}
}

func TestWHEPInitialKeyframeGateRequiresSentCachedKeyframe(t *testing.T) {
	if whepInitialKeyframeReady("live", false) {
		t.Fatal("live mode bypassed keyframe gate without sending a cached keyframe")
	}
	if !whepInitialKeyframeReady("live", true) {
		t.Fatal("live mode did not accept a successfully sent cached keyframe")
	}
	if whepInitialKeyframeReady("realtime", true) {
		t.Fatal("realtime mode bypassed keyframe gate")
	}
}

func TestWHEPInitialMediaGateAllowsAudioOnlyStreams(t *testing.T) {
	if !whepInitialMediaReady("realtime", false, false) {
		t.Fatal("audio-only realtime playback waited for a video keyframe")
	}
	if !whepInitialMediaReady("live", false, false) {
		t.Fatal("audio-only live playback waited for a video keyframe")
	}
	if whepInitialMediaReady("realtime", false, true) {
		t.Fatal("video realtime playback bypassed the keyframe gate")
	}
}

func TestWHEPFeedReadersDrainOnlyTargetAudioAfterKeyframe(t *testing.T) {
	stream := core.NewStream("live/whep-target-audio", config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{
		id: "source", info: &avframe.MediaInfo{AudioCodec: avframe.CodecAAC},
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := stream.StartupSnapshot()
	targetRing := util.NewRingBuffer[*avframe.AVFrame](16)
	readers := &whepFeedReaders{targetAudio: targetRing.NewReaderAt(0)}
	video := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe, 0, 0, []byte{1})
	aac := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, []byte{2})
	earlyOpus := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 0, 0, []byte{3})
	targetRing.Write(video)
	targetRing.Write(aac)
	targetRing.Write(earlyOpus)

	var delivered []*avframe.AVFrame
	readers.drainTargetAudio(stream, snapshot.Generation, false, avframe.CodecOpus, func(frame *avframe.AVFrame) {
		delivered = append(delivered, frame)
	})
	if len(delivered) != 0 {
		t.Fatalf("target audio delivered before keyframe: %v", delivered)
	}

	lateOpus := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 20, 20, []byte{4})
	targetRing.Write(video)
	targetRing.Write(aac)
	targetRing.Write(lateOpus)
	readers.drainTargetAudio(stream, snapshot.Generation, true, avframe.CodecOpus, func(frame *avframe.AVFrame) {
		delivered = append(delivered, frame)
	})
	if len(delivered) != 1 || delivered[0] != lateOpus {
		t.Fatalf("delivered target frames = %v, want late Opus only", delivered)
	}
}

func TestWHEPTranscodeReaderStopsBeforeReplacementGenerationFrame(t *testing.T) {
	stream := core.NewStream("live/whep-transcode-generation", config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{
		id: "old", info: &avframe.MediaInfo{AudioCodec: avframe.CodecAAC},
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := stream.StartupSnapshot()
	targetRing := util.NewRingBuffer[*avframe.AVFrame](16)
	readers := &whepFeedReaders{targetAudio: targetRing.NewReaderAt(0)}
	oldFrame := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 0, 0, []byte{1})
	replacementFrame := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 20, 20, []byte{2})
	targetRing.Write(oldFrame)

	var delivered []*avframe.AVFrame
	readers.drainTargetAudio(stream, snapshot.Generation, true, avframe.CodecOpus, func(frame *avframe.AVFrame) {
		delivered = append(delivered, frame)
		stream.RemovePublisher()
		if err := stream.SetPublisher(&authorizationTestPublisher{
			id: "replacement", info: &avframe.MediaInfo{AudioCodec: avframe.CodecAAC},
		}); err != nil {
			t.Fatal(err)
		}
		targetRing.Write(replacementFrame)
	})
	if len(delivered) != 1 || delivered[0] != oldFrame {
		t.Fatalf("delivered target frames = %v, want old-generation frame only", delivered)
	}
}

func TestWHEPAudioFrameMustMatchNegotiatedCodec(t *testing.T) {
	aac := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, []byte{1})
	opus := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 0, 0, []byte{2})

	if whepAudioFrameAllowed(aac, avframe.CodecOpus) {
		t.Fatal("AAC source frame accepted for negotiated Opus track")
	}
	if !whepAudioFrameAllowed(opus, avframe.CodecOpus) {
		t.Fatal("Opus frame rejected for negotiated Opus track")
	}
}

// TestDTSPaceDecision tests the pacing decision logic extracted from
// the feed loop. This validates the simplified behavior:
//   - sleepDur > 0 && < 1s  → should sleep
//   - sleepDur in [-1s, 0]  → should deliver immediately (no drop)
//   - |sleepDur| > 1s       → should reset pace base
func TestDTSPaceDecision(t *testing.T) {
	tests := []struct {
		name       string
		sleepDur   time.Duration
		wantAction string // "sleep", "deliver", "reset"
	}{
		{"ahead_40ms", 40 * time.Millisecond, "sleep"},
		{"ahead_500ms", 500 * time.Millisecond, "sleep"},
		{"exactly_on_time", 0, "deliver"},
		{"behind_40ms", -40 * time.Millisecond, "deliver"},
		{"behind_200ms", -200 * time.Millisecond, "deliver"},
		{"behind_500ms", -500 * time.Millisecond, "deliver"},
		{"behind_999ms", -999 * time.Millisecond, "deliver"},
		{"behind_1001ms", -1001 * time.Millisecond, "reset"},
		{"ahead_1001ms", 1001 * time.Millisecond, "reset"},
		{"behind_2s", -2 * time.Second, "reset"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dtsPaceAction(tt.sleepDur)
			if got != tt.wantAction {
				t.Errorf("dtsPaceAction(%v) = %q, want %q", tt.sleepDur, got, tt.wantAction)
			}
		})
	}
}

func TestWHEPVideoBFramePolicyIsCodecSpecific(t *testing.T) {
	bFrame := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe, 40, 33, []byte{1})

	if shouldDropWHEPVideoFrame(avframe.CodecH265, bFrame, 120) {
		t.Fatal("H.265 B-frame dropped; HEVC reference B-frames must reach the decoder")
	}
	if !shouldDropWHEPVideoFrame(avframe.CodecH264, bFrame, 120) {
		t.Fatal("H.264 B-frame retained; Chromium's H.264 WebRTC path requires it to be dropped")
	}

	keyframe := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 40, 33, []byte{2})
	if shouldDropWHEPVideoFrame(avframe.CodecH264, keyframe, 120) {
		t.Fatal("H.264 keyframe dropped because its PTS moved backward")
	}
}
