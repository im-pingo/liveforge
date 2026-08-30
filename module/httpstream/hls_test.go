package httpstream

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

func TestHLSVideoOverwriteRecoversAtKeyframeWithDiscontinuity(t *testing.T) {
	stream := newVideoStreamWithoutGOPCache(t)
	mgr := NewHLSManager(stream.Key(), "/live/hls-overwrite", 6, 8)
	input := newControlledSegmentInput(2, false)
	mgr.inputFactory = input.factory
	mgr.beforeLiveRead = input.beforeRead(mgr.done)

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	done := make(chan struct{})
	go func() {
		mgr.Run(stream)
		close(done)
	}()
	t.Cleanup(func() {
		mgr.Stop()
		input.ring.Close()
		<-done
	})

	frame := func(frameType avframe.FrameType, dts int64, marker byte) *avframe.AVFrame {
		nalType := byte(0x41)
		if frameType.IsKeyframe() {
			nalType = 0x65
		}
		return avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, frameType,
			dts, dts, []byte{0, 0, 0, 2, nalType, marker},
		)
	}

	input.writeAndRead(t, frame(avframe.FrameTypeKeyframe, 0, 0x10))
	input.writeAndRead(t, frame(avframe.FrameTypeInterframe, 500, 0x11))
	input.writeAndRead(t, frame(avframe.FrameTypeKeyframe, 1000, 0x20))
	input.writeAndRead(t, frame(avframe.FrameTypeInterframe, 1200, 0x21))
	input.writeBurstAndRead(t,
		frame(avframe.FrameTypeInterframe, 1300, 0x31),
		frame(avframe.FrameTypeInterframe, 1400, 0x32),
		frame(avframe.FrameTypeInterframe, 1500, 0x33),
		frame(avframe.FrameTypeInterframe, 1600, 0x34),
	)
	input.writeBurstAndRead(t,
		frame(avframe.FrameTypeInterframe, 1620, 0x35),
		frame(avframe.FrameTypeInterframe, 1640, 0x36),
		frame(avframe.FrameTypeInterframe, 1660, 0x37),
		frame(avframe.FrameTypeInterframe, 1680, 0x38),
	)
	input.writeAndRead(t, frame(avframe.FrameTypeInterframe, 1700, 0x40))
	input.writeAndRead(t, frame(avframe.FrameTypeKeyframe, 2000, 0x50))
	input.writeAndRead(t, frame(avframe.FrameTypeInterframe, 2200, 0x51))
	input.writeAndRead(t, frame(avframe.FrameTypeKeyframe, 3000, 0x60))
	input.waitReady(t)

	if got := mgr.SegmentCount(); got != 2 {
		t.Fatalf("HLS segments after overwrite = %d, want prior and recovered segments", got)
	}
	prior, ok := mgr.GetSegment(0)
	if !ok {
		t.Fatal("completed pre-gap HLS segment was not retained")
	}
	recovered, ok := mgr.GetSegment(1)
	if !ok {
		t.Fatal("first recovered HLS segment is unavailable")
	}
	if len(recovered) < 2*ts.PacketSize || tsPacketPID(recovered[:ts.PacketSize]) != ts.PIDPat ||
		tsPacketPID(recovered[ts.PacketSize:2*ts.PacketSize]) != ts.PIDPmt {
		t.Fatal("first recovered HLS segment does not begin with PAT/PMT")
	}
	priorVideo := demuxTSVideoFrames(prior)
	recoveredVideo := demuxTSVideoFrames(recovered)
	if len(priorVideo) == 0 || len(recoveredVideo) == 0 {
		t.Fatalf("demuxed HLS video frames before/after overwrite = %d/%d", len(priorVideo), len(recoveredVideo))
	}
	if !recoveredVideo[0].FrameType.IsKeyframe() || !bytes.Contains(recoveredVideo[0].Payload, []byte{0x65, 0x50}) {
		t.Fatalf("first recovered HLS video frame = %+v, want recovery IDR marker", recoveredVideo[0])
	}
	for _, segment := range [][]byte{prior, recovered} {
		for _, marker := range []byte{0x20, 0x21, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38, 0x40} {
			if bytes.Contains(segment, []byte{0x41, marker}) || bytes.Contains(segment, []byte{0x65, marker}) {
				t.Fatalf("HLS output retained abandoned/post-gap marker %#x", marker)
			}
		}
	}

	playlist := mgr.GenerateM3U8()
	if strings.Count(playlist, "#EXT-X-DISCONTINUITY\n") != 1 {
		t.Fatalf("HLS discontinuity count = %d, playlist:\n%s", strings.Count(playlist, "#EXT-X-DISCONTINUITY\n"), playlist)
	}
	if !strings.Contains(playlist, "/0.ts\n#EXT-X-DISCONTINUITY\n#EXTINF:") || !strings.Contains(playlist, "/1.ts\n") {
		t.Fatalf("HLS recovered sequence/discontinuity ordering is wrong:\n%s", playlist)
	}
	logText := logs.String()
	for _, field := range []string{"format=hls", "consumer=segmenter", "action=wait_keyframe", "overwritten=2"} {
		if !strings.Contains(logText, field) {
			t.Fatalf("HLS overwrite transition log missing %q: %s", field, logText)
		}
	}
}

func TestHLSAudioOnlyOverwriteResumesAtNextAudio(t *testing.T) {
	stream := newAudioOnlyAACStream(t, "live/hls-audio-overwrite")
	mgr := NewHLSManager(stream.Key(), "/live/hls-audio-overwrite", 0.1, 8)
	input := newControlledSegmentInput(2, false)
	mgr.inputFactory = input.factory
	mgr.beforeLiveRead = input.beforeRead(mgr.done)
	done := make(chan struct{})
	go func() {
		mgr.Run(stream)
		close(done)
	}()
	t.Cleanup(func() {
		mgr.Stop()
		input.ring.Close()
		<-done
	})

	audio := func(dts int64, marker byte) *avframe.AVFrame {
		return avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
			dts, dts, []byte{0x21, marker, 0x34, 0x55},
		)
	}
	for dts := int64(0); dts <= 120; dts += 20 {
		input.writeAndRead(t, audio(dts, byte(dts/20)))
	}
	input.writeBurstAndRead(t, audio(140, 0x31), audio(160, 0x32), audio(180, 0x33), audio(200, 0x34))
	for i, marker := range []byte{0x50, 0x51, 0x52, 0x53, 0x54, 0x55} {
		input.writeAndRead(t, audio(300+int64(i)*20, marker))
	}
	input.waitReady(t)

	if got := mgr.SegmentCount(); got != 2 {
		t.Fatalf("audio-only HLS segments after overwrite = %d, want prior and recovered segments", got)
	}
	recovered, ok := mgr.GetSegment(1)
	if !ok {
		t.Fatal("recovered audio-only HLS segment is unavailable")
	}
	frames := demuxTSAudioFrames(recovered)
	if len(frames) == 0 || !bytes.Equal(frames[0].Payload, []byte{0x21, 0x50, 0x34, 0x55}) {
		t.Fatalf("first recovered audio payload = %v, want next live audio marker", frames)
	}
	if got := strings.Count(mgr.GenerateM3U8(), "#EXT-X-DISCONTINUITY\n"); got != 1 {
		t.Fatalf("audio-only HLS discontinuity count = %d, want 1", got)
	}
}

func tsPacketPID(packet []byte) uint16 {
	return uint16(packet[1]&0x1f)<<8 | uint16(packet[2])
}

func demuxTSVideoFrames(segment []byte) []*avframe.AVFrame {
	var frames []*avframe.AVFrame
	demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
		if frame.MediaType.IsVideo() && frame.FrameType != avframe.FrameTypeSequenceHeader {
			frames = append(frames, frame)
		}
	})
	demuxer.Feed(segment)
	demuxer.Flush()
	return frames
}

func TestHLSManagerGenerateM3U8(t *testing.T) {
	mgr := NewHLSManager("live/test", "/live/test", 6.0, 3)

	// Empty playlist
	m3u8 := mgr.GenerateM3U8()
	if m3u8 == "" {
		t.Error("expected non-empty playlist")
	}

	// Add segments
	mgr.mu.Lock()
	mgr.segments = []*HLSSegment{
		{SeqNum: 0, Duration: 5.5, Data: []byte("seg0")},
		{SeqNum: 1, Duration: 6.1, Data: []byte("seg1")},
	}
	mgr.nextSeqNum = 2
	mgr.mu.Unlock()

	m3u8 = mgr.GenerateM3U8()
	if m3u8 == "" {
		t.Error("expected non-empty playlist with segments")
	}
}

func TestHLSManagerGetSegment(t *testing.T) {
	mgr := NewHLSManager("live/test", "/live/test", 6.0, 3)

	mgr.mu.Lock()
	mgr.segments = []*HLSSegment{
		{SeqNum: 5, Duration: 6.0, Data: []byte("seg-5")},
		{SeqNum: 6, Duration: 6.0, Data: []byte("seg-6")},
	}
	mgr.seqBase = 5
	mgr.mu.Unlock()

	data, ok := mgr.GetSegment(5)
	if !ok || string(data) != "seg-5" {
		t.Errorf("GetSegment(5) = %q, %v", data, ok)
	}

	data, ok = mgr.GetSegment(6)
	if !ok || string(data) != "seg-6" {
		t.Errorf("GetSegment(6) = %q, %v", data, ok)
	}

	_, ok = mgr.GetSegment(99)
	if ok {
		t.Error("GetSegment(99) should not find")
	}
}

func TestHLSManagerSegmentCount(t *testing.T) {
	mgr := NewHLSManager("live/test", "/live/test", 6.0, 3)

	if mgr.SegmentCount() != 0 {
		t.Errorf("expected 0, got %d", mgr.SegmentCount())
	}

	mgr.mu.Lock()
	mgr.segments = []*HLSSegment{{SeqNum: 0}, {SeqNum: 1}}
	mgr.mu.Unlock()

	if mgr.SegmentCount() != 2 {
		t.Errorf("expected 2, got %d", mgr.SegmentCount())
	}
}

func TestHLSManagerAudioOnlyProducesLiveSegments(t *testing.T) {
	stream := newAudioOnlyAACStream(t, "live/hls-audio-only")
	mgr := NewHLSManager(stream.Key(), "/live/hls-audio-only", 0.2, 5)
	done := make(chan struct{})
	go func() {
		mgr.Run(stream)
		close(done)
	}()
	t.Cleanup(func() {
		mgr.Stop()
		stream.RingBuffer().Close()
		<-done
	})

	time.Sleep(20 * time.Millisecond)
	payloads := writeLiveAACFrames(stream, 22, 20)
	waitForSegmentCount(t, mgr.SegmentCount, 2)
	select {
	case <-done:
		t.Fatal("HLS manager stopped before the live source")
	default:
	}

	first, ok := mgr.GetSegment(0)
	if !ok {
		t.Fatal("first audio-only HLS segment is unavailable while source is live")
	}
	second, ok := mgr.GetSegment(1)
	if !ok {
		t.Fatal("second audio-only HLS segment is unavailable while source is live")
	}
	firstFrames := demuxTSAudioFrames(first)
	secondFrames := demuxTSAudioFrames(second)
	if len(firstFrames) == 0 || len(secondFrames) == 0 {
		t.Fatalf("audio-only HLS demuxed frames = %d/%d, want audio in both segments", len(firstFrames), len(secondFrames))
	}
	assertBoundaryPayloadStartsNextSegmentOnce(t, firstFrames, secondFrames, payloads[10])
}

func TestCopyBytesAndBufCopyAndReset(t *testing.T) {
	src := []byte("hello world")
	dst := copyBytes(src)
	if string(dst) != "hello world" {
		t.Errorf("copyBytes: got %q", dst)
	}
	// Modify src, dst should be independent
	src[0] = 'H'
	if dst[0] == 'H' {
		t.Error("copyBytes should return independent copy")
	}
}

func TestParseAudioSeqHeader(t *testing.T) {
	// AAC-LC, 44100 Hz, stereo: first 2 bytes = 0x12 0x10
	// objectType=2 (AAC-LC), samplingFrequencyIndex=4 (44100), channelConfiguration=2
	payload := []byte{0x12, 0x10}
	sr, ch := parseAudioSeqHeader(&avframe.AVFrame{Codec: avframe.CodecAAC, Payload: payload})
	if sr != 44100 {
		t.Errorf("sample rate: got %d, want 44100", sr)
	}
	if ch != 2 {
		t.Errorf("channels: got %d, want 2", ch)
	}

	// Invalid payload
	sr, ch = parseAudioSeqHeader(&avframe.AVFrame{Codec: avframe.CodecAAC, Payload: []byte{0xFF}})
	if sr != 0 || ch != 0 {
		t.Errorf("invalid payload: got sr=%d ch=%d, want 0,0", sr, ch)
	}
}

func TestHLSManagerDefaults(t *testing.T) {
	mgr := NewHLSManager("test", "/test", 0, 0)
	// Should use defaults: targetDur=6.0, maxSegments=5
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	if mgr.targetDur != 6.0 {
		t.Errorf("default targetDur: got %f, want 6.0", mgr.targetDur)
	}
	if mgr.maxSegments != 5 {
		t.Errorf("default maxSegments: got %d, want 5", mgr.maxSegments)
	}
}

func TestDASHManagerDefaults(t *testing.T) {
	mgr := NewDASHManager("test", "/test", 0, 0)
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	if mgr.targetDur != 6.0 {
		t.Errorf("default targetDur: got %f, want 6.0", mgr.targetDur)
	}
	if mgr.maxSegments != 30 {
		t.Errorf("default maxSegments: got %d, want 30", mgr.maxSegments)
	}
}

func TestBufCopyAndReset(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte("test data"))
	result := bufCopyAndReset(&buf)
	if string(result) != "test data" {
		t.Errorf("got %q", result)
	}
	if buf.Len() != 0 {
		t.Error("buffer should be reset")
	}
}

func TestHLSManagerDropsOpusWhenAudioTranscodingIsUnavailable(t *testing.T) {
	stream := newMuxerWorkerStream(t, avframe.CodecOpus)
	mgr := NewHLSManager("live/hls-opus", "/live/hls-opus", 1, 5)
	done := make(chan struct{})
	go func() {
		mgr.Run(stream)
		close(done)
	}()
	t.Cleanup(func() {
		mgr.Stop()
		stream.RingBuffer().Close()
		<-done
	})

	// Let Run capture its atomic GOP/cache cursor before adding live frames.
	time.Sleep(20 * time.Millisecond)

	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe,
		20, 20, []byte{0xf8, 0xff, 0xfe},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		500, 500, []byte{0, 0, 0, 2, 0x41, 0x01},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		1000, 1000, []byte{0, 0, 0, 2, 0x65, 0x01},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		2000, 2000, []byte{0, 0, 0, 2, 0x65, 0x02},
	))

	deadline := time.Now().Add(2 * time.Second)
	for mgr.SegmentCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if mgr.SegmentCount() < 2 {
		t.Fatal("HLS manager did not finalize two keyframe-bounded segments")
	}
	for seqNum := range 2 {
		segment, ok := mgr.GetSegment(seqNum)
		if !ok {
			t.Fatalf("HLS segment %d is missing", seqNum)
		}
		for offset := 0; offset+ts.PacketSize <= len(segment); offset += ts.PacketSize {
			packet := segment[offset : offset+ts.PacketSize]
			pid := uint16(packet[1]&0x1f)<<8 | uint16(packet[2])
			if pid == ts.PIDAudio {
				t.Fatalf("HLS segment %d contains an audio PID for unsupported Opus", seqNum)
			}
		}
	}
}

func TestHLSManagerCachedGOPContinuesWithLiveInterframes(t *testing.T) {
	stream := newMuxerWorkerStream(t, 0)
	mgr := NewHLSManager("live/hls-keyframe", "/live/hls-keyframe", 1, 5)
	done := make(chan struct{})
	go func() {
		mgr.Run(stream)
		close(done)
	}()
	t.Cleanup(func() {
		mgr.Stop()
		stream.RingBuffer().Close()
		<-done
	})

	// Let Run capture its atomic GOP/cache cursor before adding live frames.
	time.Sleep(20 * time.Millisecond)

	for _, frame := range []*avframe.AVFrame{
		avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
			500, 500, []byte{0, 0, 0, 2, 0x41, 0x01},
		),
		avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
			1000, 1000, []byte{0, 0, 0, 2, 0x65, 0x01},
		),
		avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
			1500, 1500, []byte{0, 0, 0, 2, 0x41, 0x02},
		),
		avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
			2000, 2000, []byte{0, 0, 0, 2, 0x65, 0x02},
		),
	} {
		stream.WriteFrame(frame)
	}

	deadline := time.Now().Add(2 * time.Second)
	for mgr.SegmentCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	segment, ok := mgr.GetSegment(0)
	if !ok {
		t.Fatal("HLS segment 0 is missing")
	}

	var firstVideo *avframe.AVFrame
	foundSnapshotLiveFrame := false
	demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
		if firstVideo == nil && frame.MediaType.IsVideo() && frame.FrameType != avframe.FrameTypeSequenceHeader {
			firstVideo = frame
		}
		if frame.MediaType.IsVideo() && frame.FrameType != avframe.FrameTypeSequenceHeader && frame.DTS == 500 {
			foundSnapshotLiveFrame = true
		}
	})
	demuxer.Feed(segment)
	demuxer.Flush()
	if firstVideo == nil {
		t.Fatal("HLS live segment contains no video sample")
	}
	if !firstVideo.FrameType.IsKeyframe() {
		t.Fatalf("HLS segment starts with %v, want keyframe", firstVideo.FrameType)
	}
	if !foundSnapshotLiveFrame {
		t.Fatal("HLS segment 0 dropped the live interframe immediately after the cache snapshot")
	}
}

func TestHLSManagerDoesNotUseAudioDTSAsLiveVideoWatermark(t *testing.T) {
	stream := newMuxerWorkerStream(t, avframe.CodecAAC)
	// Make the last cached frame audio several seconds ahead of the cached
	// video. A cross-track DTS watermark would incorrectly discard the live
	// video frame at 100ms even though the atomic snapshot cursor proves it is
	// not part of the cache.
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
		4000, 4000, []byte{0x01, 0x02, 0x03},
	))

	mgr := NewHLSManager("live/hls-cross-track-dts", "/live/hls-cross-track-dts", 1, 5)
	done := make(chan struct{})
	go func() {
		mgr.Run(stream)
		close(done)
	}()
	t.Cleanup(func() {
		mgr.Stop()
		stream.RingBuffer().Close()
		<-done
	})

	// Give Run time to capture the cache and cursor, then append frames that
	// must be read from the live ring.
	time.Sleep(20 * time.Millisecond)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		100, 100, []byte{0, 0, 0, 2, 0x41, 0x7a},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		5000, 5000, []byte{0, 0, 0, 2, 0x65, 0x7b},
	))

	deadline := time.Now().Add(2 * time.Second)
	for mgr.SegmentCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	segment, ok := mgr.GetSegment(0)
	if !ok {
		t.Fatal("HLS segment 0 is missing")
	}
	foundLiveInterframe := false
	demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
		if frame.MediaType.IsVideo() && frame.FrameType == avframe.FrameTypeInterframe &&
			bytes.Contains(frame.Payload, []byte{0x41, 0x7a}) {
			foundLiveInterframe = true
		}
	})
	demuxer.Feed(segment)
	demuxer.Flush()
	if !foundLiveInterframe {
		t.Fatal("HLS dropped the live video frame because cached audio had a later DTS")
	}
}

func TestHLSManagerFirstSegmentStartsAtFirstLiveKeyframeWithoutCache(t *testing.T) {
	stream := newVideoStreamWithoutGOPCache(t)
	mgr := NewHLSManager("live/hls-no-cache", "/live/hls-no-cache", 6, 5)
	done := make(chan struct{})
	go func() {
		mgr.Run(stream)
		close(done)
	}()
	t.Cleanup(func() {
		mgr.Stop()
		stream.RingBuffer().Close()
		<-done
	})

	time.Sleep(20 * time.Millisecond)
	for _, frame := range []*avframe.AVFrame{
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 500, 500, []byte{0, 0, 0, 2, 0x41, 0x01}),
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 1000, 1000, []byte{0, 0, 0, 2, 0x65, 0x02}),
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 1500, 1500, []byte{0, 0, 0, 2, 0x41, 0x03}),
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 2000, 2000, []byte{0, 0, 0, 2, 0x65, 0x04}),
	} {
		stream.WriteFrame(frame)
	}

	deadline := time.Now().Add(2 * time.Second)
	for mgr.SegmentCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	segment, ok := mgr.GetSegment(0)
	if !ok {
		t.Fatal("HLS first segment is missing")
	}
	var firstVideo *avframe.AVFrame
	demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
		if firstVideo == nil && frame.MediaType.IsVideo() && frame.FrameType != avframe.FrameTypeSequenceHeader {
			firstVideo = frame
		}
	})
	demuxer.Feed(segment)
	demuxer.Flush()
	if firstVideo == nil || !firstVideo.FrameType.IsKeyframe() {
		t.Fatal("HLS advertised a first segment that starts before the first live keyframe")
	}
}

func newVideoStreamWithoutGOPCache(t *testing.T) *core.Stream {
	t.Helper()
	stream := core.NewStream("live/no-cache", config.StreamConfig{
		GOPCache:       false,
		RingBufferSize: 256,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&muxerWorkerPublisher{info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
	}}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x42, 0x00, 0x1e, 0xff},
	))
	return stream
}

func newAudioOnlyAACStream(t *testing.T, key string) *core.Stream {
	t.Helper()
	stream := core.NewStream(key, config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 256,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&muxerWorkerPublisher{info: &avframe.MediaInfo{
		AudioCodec:          avframe.CodecAAC,
		AudioSequenceHeader: []byte{0x12, 0x10},
		SampleRate:          44100,
		Channels:            2,
	}}); err != nil {
		t.Fatal(err)
	}
	return stream
}

func writeLiveAACFrames(stream *core.Stream, count int, intervalMS int64) [][]byte {
	return writeLiveAACFramesFromDTS(stream, count, intervalMS, 0)
}

func writeLiveAACFramesFromDTS(stream *core.Stream, count int, intervalMS, originDTS int64) [][]byte {
	payloads := make([][]byte, count)
	for i := range count {
		payload := []byte{0x21, byte(i), 0x34, 0x55}
		payloads[i] = payload
		dts := originDTS + int64(i)*intervalMS
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
			dts, dts, payload,
		))
	}
	return payloads
}

func waitForSegmentCount(t *testing.T, count func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for count() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := count(); got < want {
		t.Fatalf("completed live segments = %d, want at least %d", got, want)
	}
}

func demuxTSAudioFrames(segment []byte) []*avframe.AVFrame {
	var frames []*avframe.AVFrame
	demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
		if frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader {
			frames = append(frames, frame)
		}
	})
	demuxer.Feed(segment)
	demuxer.Flush()
	return frames
}

func assertBoundaryPayloadStartsNextSegmentOnce(
	t *testing.T,
	first, second []*avframe.AVFrame,
	boundaryPayload []byte,
) {
	t.Helper()
	firstCount := 0
	for _, frame := range first {
		if bytes.Equal(frame.Payload, boundaryPayload) {
			firstCount++
		}
	}
	secondCount := 0
	for _, frame := range second {
		if bytes.Equal(frame.Payload, boundaryPayload) {
			secondCount++
		}
	}
	if firstCount != 0 || secondCount != 1 {
		t.Fatalf("boundary payload occurrences in previous/next segment = %d/%d, want 0/1", firstCount, secondCount)
	}
}
