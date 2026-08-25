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

func TestWHEPLiveSnapshotKeepsFramesWrittenWhileCacheIsSent(t *testing.T) {
	stream := core.NewStream("live/whep-snapshot", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 16,
	}, config.LimitsConfig{}, core.NewEventBus())
	cached := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeKeyframe, 1000, 1000, []byte{1})
	stream.WriteFrame(cached)

	gopCache, startPos := whepLiveSnapshot(stream, false)
	if len(gopCache) != 1 || gopCache[0] != cached {
		t.Fatalf("GOP snapshot = %v, want cached keyframe", gopCache)
	}

	live := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe, 1033, 1033, []byte{2})
	stream.WriteFrame(live) // Simulates a publisher write while the cached GOP is sent.
	got, ok := stream.RingBuffer().NewReaderAt(startPos).TryRead()
	if !ok || got != live {
		t.Fatalf("first live frame after GOP snapshot = (%v, %v), want newly written frame", got, ok)
	}
}

func TestWHEPLiveSnapshotDropsSourceAudioWhenTranscoding(t *testing.T) {
	stream := core.NewStream("live/whep-transcode-cache", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 16,
	}, config.LimitsConfig{}, core.NewEventBus())
	video := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeKeyframe, 1000, 1000, []byte{1})
	aac := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 1020, 1020, []byte{2})
	stream.WriteFrame(video)
	stream.WriteFrame(aac)

	gopCache, _ := whepLiveSnapshot(stream, true)
	if len(gopCache) != 1 || gopCache[0] != video {
		t.Fatalf("transcoded live GOP snapshot = %v, want cached video only", gopCache)
	}
}

func TestWHEPFeedReadersKeepAtomicSourceCursorWhenTranscoderCloses(t *testing.T) {
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

	_, startPos := whepLiveSnapshot(stream, true)
	betweenSnapshotAndReader := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe,
		1033, 1033, []byte{1},
	)
	stream.WriteFrame(betweenSnapshotAndReader)

	readers := newWHEPFeedReaders(stream, startPos, startPos, true, avframe.CodecOpus)
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

	select {
	case <-readers.targetAudio.Signal(): // Empty registry makes transcoder close asynchronously.
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for failed transcoder to close")
	}
	if _, ok := readers.targetAudio.TryRead(); ok {
		t.Fatal("failed transcoder unexpectedly produced a frame")
	}

	woke := make(chan bool, 1)
	waitDone := make(chan struct{})
	go func() {
		woke <- readers.wait(waitDone)
	}()
	select {
	case <-woke:
		t.Fatal("reader wait returned before source video arrived")
	case <-time.After(20 * time.Millisecond):
	}

	afterTranscoderClose := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe,
		1066, 1066, []byte{2},
	)
	stream.WriteFrame(afterTranscoderClose)
	select {
	case ok := <-woke:
		if !ok {
			t.Fatal("reader wait stopped after transcode failure")
		}
	case <-time.After(time.Second):
		close(waitDone)
		t.Fatal("source video did not wake reader after transcode failure")
	}
	if got, ok := readers.source.TryRead(); !ok || got != afterTranscoderClose {
		t.Fatalf("source reader after transcode failure = (%v, %v), want uninterrupted video", got, ok)
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

func TestWHEPFeedReadersDrainOnlyTargetAudioAfterKeyframe(t *testing.T) {
	targetRing := util.NewRingBuffer[*avframe.AVFrame](16)
	readers := &whepFeedReaders{targetAudio: targetRing.NewReaderAt(0)}
	video := avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe, 0, 0, []byte{1})
	aac := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, []byte{2})
	earlyOpus := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 0, 0, []byte{3})
	targetRing.Write(video)
	targetRing.Write(aac)
	targetRing.Write(earlyOpus)

	var delivered []*avframe.AVFrame
	readers.drainTargetAudio(false, avframe.CodecOpus, func(frame *avframe.AVFrame) {
		delivered = append(delivered, frame)
	})
	if len(delivered) != 0 {
		t.Fatalf("target audio delivered before keyframe: %v", delivered)
	}

	lateOpus := avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe, 20, 20, []byte{4})
	targetRing.Write(video)
	targetRing.Write(aac)
	targetRing.Write(lateOpus)
	readers.drainTargetAudio(true, avframe.CodecOpus, func(frame *avframe.AVFrame) {
		delivered = append(delivered, frame)
	})
	if len(delivered) != 1 || delivered[0] != lateOpus {
		t.Fatalf("delivered target frames = %v, want late Opus only", delivered)
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
