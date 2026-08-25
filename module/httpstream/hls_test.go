package httpstream

import (
	"bytes"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

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
