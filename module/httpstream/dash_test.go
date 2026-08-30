package httpstream

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
)

func TestDASHVideoOverwriteRetiresManagerWithoutFlushing(t *testing.T) {
	stream := newVideoStreamWithoutGOPCache(t)
	mgr := NewDASHManager(stream.Key(), "/live/dash-overwrite", 6, 8)
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

	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("DASH manager continued its single Period after input overwrite")
	}
	if !mgr.isRetired() || !mgr.isTerminal() {
		t.Fatal("DASH overwrite did not expose retired terminal state")
	}
	if got := mgr.SegmentCount(); got != 1 {
		t.Fatalf("DASH segments after overwrite = %d, want only completed pre-gap segment", got)
	}
	segment, ok := mgr.GetSegment(0)
	if !ok || len(segment) == 0 {
		t.Fatal("completed pre-gap DASH segment was not retained")
	}
	for _, marker := range []byte{0x20, 0x21, 0x31, 0x32, 0x33, 0x34} {
		if bytes.Contains(segment, []byte{0x41, marker}) || bytes.Contains(segment, []byte{0x65, marker}) {
			t.Fatalf("DASH output retained abandoned/post-gap marker %#x", marker)
		}
	}
}

func TestDASHAudioOnlyOverwriteRetiresAndEndsFutureSegmentWait(t *testing.T) {
	stream := newAudioOnlyAACStream(t, "live/dash-audio-overwrite")
	mgr := NewDASHManager(stream.Key(), "/live/dash-audio-overwrite", 0.1, 8)
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
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("audio-only DASH manager did not retire after overwrite")
	}
	if !mgr.isRetired() || mgr.SegmentCount() != 1 {
		t.Fatalf("audio-only DASH retired/count = %v/%d, want true/1", mgr.isRetired(), mgr.SegmentCount())
	}
	if _, ok := mgr.GetAudioSegment(0); !ok {
		t.Fatal("completed pre-gap audio-only DASH segment was not retained")
	}
	if _, ok := mgr.GetAudioSegment(1); ok {
		t.Fatal("audio-only DASH flushed its abandoned current batch")
	}

	module := NewModule()
	module.dashManagers[stream.Key()] = mgr
	request := httptest.NewRequest(http.MethodGet, "/future.m4s", nil)
	recorder := httptest.NewRecorder()
	started := time.Now()
	module.serveDASHAudioSegment(recorder, request, stream.Key(), 99)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("future segment after DASH retirement status = %d, want 404", recorder.Code)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("future DASH segment wait continued after terminal state for %v", elapsed)
	}
}

func TestDASHVideoCodecString(t *testing.T) {
	tests := []struct {
		name  string
		codec avframe.CodecType
		seq   *avframe.AVFrame
		want  string
	}{
		{"h264 with header", avframe.CodecH264, &avframe.AVFrame{Payload: []byte{0x01, 0x64, 0x00, 0x28}}, "avc1.640028"},
		{"h264 fallback", avframe.CodecH264, nil, "avc1.640028"},
		{"h265 fallback", avframe.CodecH265, nil, "hvc1.1.6.L120.B0"},
		{"unknown codec", avframe.CodecType(99), nil, "avc1.640028"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dashVideoCodecString(tt.codec, tt.seq)
			if got != tt.want {
				t.Errorf("dashVideoCodecString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDASHAudioCodecString(t *testing.T) {
	tests := []struct {
		name  string
		codec avframe.CodecType
		seq   *avframe.AVFrame
		want  string
	}{
		{"aac default", avframe.CodecAAC, nil, "mp4a.40.2"},
		{"opus", avframe.CodecOpus, nil, "opus"},
		{"mp3", avframe.CodecMP3, nil, "mp4a.40.34"},
		{"unknown", avframe.CodecType(99), nil, "mp4a.40.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dashAudioCodecString(tt.codec, tt.seq)
			if got != tt.want {
				t.Errorf("dashAudioCodecString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDASHManagerSegmentRange(t *testing.T) {
	mgr := NewDASHManager("test", "/test", 6.0, 5)

	// Empty case
	lo, hi := mgr.SegmentRange()
	if lo != -1 || hi != -1 {
		t.Errorf("empty: lo=%d, hi=%d, want -1, -1", lo, hi)
	}

	// Add segments
	mgr.mu.Lock()
	mgr.videoSegments = []*DASHSegment{
		{SeqNum: 3, Duration: 2.0, Data: []byte("a")},
		{SeqNum: 4, Duration: 2.0, Data: []byte("b")},
		{SeqNum: 5, Duration: 2.0, Data: []byte("c")},
	}
	mgr.mu.Unlock()

	lo, hi = mgr.SegmentRange()
	if lo != 3 || hi != 5 {
		t.Errorf("with segments: lo=%d, hi=%d, want 3, 5", lo, hi)
	}
}

func TestDASHManagerGetSegmentAndAudioSegment(t *testing.T) {
	mgr := NewDASHManager("test", "/test", 6.0, 5)

	mgr.mu.Lock()
	mgr.videoSegments = []*DASHSegment{{SeqNum: 0, Data: []byte("vid")}}
	mgr.audioSegments = []*DASHSegment{{SeqNum: 0, Data: []byte("aud")}}
	mgr.videoInitSeg = []byte("vinit")
	mgr.audioInitSeg = []byte("ainit")
	mgr.mu.Unlock()

	// Video segment
	data, ok := mgr.GetSegment(0)
	if !ok || string(data) != "vid" {
		t.Errorf("GetSegment(0) = %q, %v", data, ok)
	}
	_, ok = mgr.GetSegment(99)
	if ok {
		t.Error("GetSegment(99) should not find")
	}

	// Audio segment
	data, ok = mgr.GetAudioSegment(0)
	if !ok || string(data) != "aud" {
		t.Errorf("GetAudioSegment(0) = %q, %v", data, ok)
	}

	// Init segments
	data, ok = mgr.GetInitSegment()
	if !ok || string(data) != "vinit" {
		t.Errorf("GetInitSegment() = %q, %v", data, ok)
	}
	data, ok = mgr.GetAudioInitSegment()
	if !ok || string(data) != "ainit" {
		t.Errorf("GetAudioInitSegment() = %q, %v", data, ok)
	}
}

func TestDASHManagerAudioOnlyProducesLiveSegments(t *testing.T) {
	for _, originDTS := range []int64{0, 5000} {
		t.Run(fmt.Sprintf("origin_%d", originDTS), func(t *testing.T) {
			streamKey := fmt.Sprintf("live/dash-audio-only-%d", originDTS)
			stream := newAudioOnlyAACStream(t, streamKey)
			mgr := NewDASHManager(stream.Key(), "/"+streamKey, 0.2, 5)
			mgr.InitFromStream(stream)
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
			payloads := writeLiveAACFramesFromDTS(stream, 22, 20, originDTS)
			waitForSegmentCount(t, mgr.SegmentCount, 2)
			select {
			case <-done:
				t.Fatal("DASH manager stopped before the live source")
			default:
			}

			initData, ok := mgr.GetAudioInitSegment()
			if !ok {
				t.Fatal("audio-only DASH init segment is unavailable while source is live")
			}
			demuxer, err := fmp4.NewDemuxer(initData)
			if err != nil {
				t.Fatalf("create audio-only DASH demuxer: %v", err)
			}
			firstData, ok := mgr.GetAudioSegment(0)
			if !ok {
				t.Fatal("first audio-only DASH segment is unavailable while source is live")
			}
			secondData, ok := mgr.GetAudioSegment(1)
			if !ok {
				t.Fatal("second audio-only DASH segment is unavailable while source is live")
			}
			firstFrames, err := demuxer.Parse(firstData)
			if err != nil {
				t.Fatalf("demux first audio-only DASH segment: %v", err)
			}
			secondFrames, err := demuxer.Parse(secondData)
			if err != nil {
				t.Fatalf("demux second audio-only DASH segment: %v", err)
			}
			if len(firstFrames) == 0 || len(secondFrames) == 0 {
				t.Fatalf("audio-only DASH demuxed frames = %d/%d, want audio in both segments", len(firstFrames), len(secondFrames))
			}
			if firstFrames[0].DTS != 0 || secondFrames[0].DTS != 200 {
				t.Fatalf("audio-only DASH demuxed segment starts = %d/%d ms, want 0/200 ms", firstFrames[0].DTS, secondFrames[0].DTS)
			}
			if firstBase, secondBase := dashTFDT(t, firstData), dashTFDT(t, secondData); firstBase != 0 || secondBase != 8820 {
				t.Fatalf("audio-only DASH tfdt values = %d/%d, want 0/8820", firstBase, secondBase)
			}
			assertBoundaryPayloadStartsNextSegmentOnce(t, firstFrames, secondFrames, payloads[10])
			mpd := mgr.GenerateMPD()
			if !strings.Contains(mpd, `contentType="audio"`) || strings.Contains(mpd, `contentType="video"`) {
				t.Fatalf("audio-only DASH MPD advertised the wrong adaptations:\n%s", mpd)
			}
		})
	}
}

func dashTFDT(t *testing.T, segment []byte) uint64 {
	t.Helper()
	for offset := 0; offset+8 <= len(segment); {
		size := int(binary.BigEndian.Uint32(segment[offset : offset+4]))
		if size < 8 || offset+size > len(segment) {
			t.Fatal("invalid top-level DASH fragment box")
		}
		if string(segment[offset+4:offset+8]) == "moof" {
			return dashTFDTInBoxes(t, segment[offset+8:offset+size])
		}
		offset += size
	}
	t.Fatal("DASH fragment is missing moof")
	return 0
}

func dashTFDTInBoxes(t *testing.T, boxes []byte) uint64 {
	t.Helper()
	for offset := 0; offset+8 <= len(boxes); {
		size := int(binary.BigEndian.Uint32(boxes[offset : offset+4]))
		if size < 8 || offset+size > len(boxes) {
			t.Fatal("invalid nested DASH fragment box")
		}
		boxType := string(boxes[offset+4 : offset+8])
		if boxType == "tfdt" {
			if size < 20 || boxes[offset+8] != 1 {
				t.Fatal("DASH tfdt is not a complete version 1 box")
			}
			return binary.BigEndian.Uint64(boxes[offset+12 : offset+20])
		}
		if boxType == "traf" {
			return dashTFDTInBoxes(t, boxes[offset+8:offset+size])
		}
		offset += size
	}
	t.Fatal("DASH fragment is missing tfdt")
	return 0
}

func TestDASHManagerGenerateMPD(t *testing.T) {
	mgr := NewDASHManager("live/test", "/live/test", 6.0, 5)

	mgr.mu.Lock()
	mgr.videoCodecStr = "avc1.640028"
	mgr.videoWidth = 1920
	mgr.videoHeight = 1080
	mgr.hasAudio = true
	mgr.audioCodec = "mp4a.40.2"
	mgr.audioSampleRate = 44100
	mgr.videoSegments = []*DASHSegment{
		{SeqNum: 0, Duration: 6.0, Data: []byte("seg0")},
		{SeqNum: 1, Duration: 6.0, Data: []byte("seg1")},
	}
	mgr.audioSegments = []*DASHSegment{
		{SeqNum: 0, Duration: 6.0, Data: []byte("aseg0")},
		{SeqNum: 1, Duration: 6.0, Data: []byte("aseg1")},
	}
	mgr.nextSeqNum = 2
	mgr.mu.Unlock()

	mpd := mgr.GenerateMPD()
	if mpd == "" {
		t.Fatal("expected non-empty MPD")
	}
	if !strings.Contains(mpd, "avc1.640028") {
		t.Error("MPD should contain video codec string")
	}
	if !strings.Contains(mpd, "mp4a.40.2") {
		t.Error("MPD should contain audio codec string")
	}
	if !strings.Contains(mpd, "1920") {
		t.Error("MPD should contain video width")
	}
}

func TestDASHManagerStopIdempotent(t *testing.T) {
	mgr := NewDASHManager("live/test", "/live/test", 6.0, 5)
	mgr.Stop()
	mgr.Stop() // should not panic
}

func TestDASHManagerGenerateMPDVideoOnly(t *testing.T) {
	mgr := NewDASHManager("live/test", "/live/test", 6.0, 5)

	mgr.mu.Lock()
	mgr.videoCodecStr = "avc1.640028"
	mgr.videoWidth = 1280
	mgr.videoHeight = 720
	mgr.hasAudio = false
	mgr.videoSegments = []*DASHSegment{
		{SeqNum: 0, Duration: 6.0, Data: []byte("seg0")},
	}
	mgr.nextSeqNum = 1
	mgr.mu.Unlock()

	mpd := mgr.GenerateMPD()
	if !strings.Contains(mpd, "avc1.640028") {
		t.Error("MPD should contain video codec")
	}
	if strings.Contains(mpd, "mp4a") {
		t.Error("video-only MPD should not contain audio codec")
	}
}

func TestDASHManagerEmptyInitSegment(t *testing.T) {
	mgr := NewDASHManager("live/test", "/live/test", 6.0, 5)

	_, ok := mgr.GetInitSegment()
	if ok {
		t.Error("empty manager should not have init segment")
	}
	_, ok = mgr.GetAudioInitSegment()
	if ok {
		t.Error("empty manager should not have audio init segment")
	}
}

func TestDASHManagerSynthesizesWHIPOpusTrackConfiguration(t *testing.T) {
	stream := newMuxerWorkerStream(t, avframe.CodecOpus)
	mgr := NewDASHManager("live/dash-opus", "/live/dash-opus", 1, 5)
	mgr.InitFromStream(stream)

	initData, ok := mgr.GetAudioInitSegment()
	if !ok {
		t.Fatal("DASH manager did not create an Opus audio init segment")
	}
	if !bytes.Contains(initData, []byte("Opus")) || !bytes.Contains(initData, []byte("dOps")) {
		t.Fatal("DASH Opus init segment is missing Opus/dOps boxes")
	}
	mgr.mu.RLock()
	hasAudio, audioCodec, sampleRate := mgr.hasAudio, mgr.audioCodec, mgr.audioSampleRate
	mgr.mu.RUnlock()
	if !hasAudio || audioCodec != "opus" || sampleRate != 48000 {
		t.Fatalf("DASH Opus metadata = hasAudio:%v codec:%q sampleRate:%d", hasAudio, audioCodec, sampleRate)
	}
}

func TestDASHManagerDerivesH265TrackDimensions(t *testing.T) {
	stream := newH265MuxerWorkerStream(t)
	mgr := NewDASHManager("live/dash-h265", "/live/dash-h265", 1, 5)
	mgr.InitFromStream(stream)

	initData, ok := mgr.GetInitSegment()
	if !ok {
		t.Fatal("DASH manager did not create an H.265 video init segment")
	}
	assertH265SampleEntryDimensions(t, initData, 640, 480)

	mgr.mu.RLock()
	width, height := mgr.videoWidth, mgr.videoHeight
	mgr.mu.RUnlock()
	if width != 640 || height != 480 {
		t.Fatalf("DASH H.265 representation dimensions = %dx%d, want 640x480", width, height)
	}
}

func TestDASHManagerH265CodecMatchesInitSegment(t *testing.T) {
	stream := newH265MuxerWorkerStream(t)
	mgr := NewDASHManager("live/dash-h265-codec", "/live/dash-h265-codec", 1, 5)
	mgr.InitFromStream(stream)

	initData, ok := mgr.GetInitSegment()
	if !ok {
		t.Fatal("DASH manager did not create an H.265 video init segment")
	}
	if !bytes.Contains(initData, []byte("hvc1")) || bytes.Contains(initData, []byte("hev1")) {
		t.Fatal("DASH H.265 init segment sample entry is not hvc1")
	}

	mgr.mu.RLock()
	codec := mgr.videoCodecStr
	mgr.mu.RUnlock()
	if codec != "hvc1.1.6.L90.B0" {
		t.Fatalf("DASH H.265 codec string = %q, want %q", codec, "hvc1.1.6.L90.B0")
	}
	if mpd := mgr.GenerateMPD(); !strings.Contains(mpd, `codecs="hvc1.1.6.L90.B0"`) {
		t.Fatalf("DASH MPD codec does not match the hvc1 init segment: %s", mpd)
	}
}

func TestDASHManagerVersionsInitSegmentURLs(t *testing.T) {
	mgr := NewDASHManager("live/versioned", "/live/versioned", 1, 5)
	mgr.mu.Lock()
	mgr.videoInitSeg = []byte("video configuration")
	mgr.audioInitSeg = []byte("audio configuration")
	mgr.hasVideo = true
	mgr.videoCodecStr = "hvc1.1.6.L90.B0"
	mgr.videoWidth = 640
	mgr.videoHeight = 480
	mgr.hasAudio = true
	mgr.audioCodec = "opus"
	mgr.audioSampleRate = 48000
	mgr.audioSegments = []*DASHSegment{{SeqNum: 0, Duration: 1, Data: []byte("audio segment")}}
	mgr.mu.Unlock()

	mpd := mgr.GenerateMPD()
	if !strings.Contains(mpd, `vinit.mp4?v=`) {
		t.Fatal("DASH MPD video init URL is not versioned")
	}
	if !strings.Contains(mpd, `audio_init.mp4?v=`) {
		t.Fatal("DASH MPD audio init URL is not versioned")
	}
}

func TestDASHManagerLongGOPManifestRefreshesPromptlyAndKeepsTimeline(t *testing.T) {
	mgr := NewDASHManager("live/long-gop", "/live/long-gop", 6, 5)
	mgr.mu.Lock()
	mgr.videoSegments = []*DASHSegment{
		{SeqNum: 0, Duration: 8.3, Data: []byte("segment")},
	}
	mgr.mu.Unlock()

	var manifest struct {
		MinimumUpdatePeriod string `xml:"minimumUpdatePeriod,attr"`
		Period              struct {
			AdaptationSets []struct {
				ContentType     string `xml:"contentType,attr"`
				SegmentTemplate struct {
					Timeline struct {
						Segments []struct {
							Duration int64 `xml:"d,attr"`
						} `xml:"S"`
					} `xml:"SegmentTimeline"`
				} `xml:"SegmentTemplate"`
			} `xml:"AdaptationSet"`
		} `xml:"Period"`
	}
	if err := xml.Unmarshal([]byte(mgr.GenerateMPD()), &manifest); err != nil {
		t.Fatalf("parse MPD: %v", err)
	}
	if manifest.MinimumUpdatePeriod != "PT2S" {
		t.Fatalf("minimumUpdatePeriod = %q, want PT2S for timely long-GOP discovery", manifest.MinimumUpdatePeriod)
	}
	if len(manifest.Period.AdaptationSets) == 0 ||
		len(manifest.Period.AdaptationSets[0].SegmentTemplate.Timeline.Segments) != 1 {
		t.Fatal("video SegmentTimeline does not contain the available segment")
	}
	if got := manifest.Period.AdaptationSets[0].SegmentTemplate.Timeline.Segments[0].Duration; got != 8300 {
		t.Fatalf("SegmentTimeline duration = %d, want 8300ms", got)
	}
}

func TestDASHManagerFirstSegmentStartsAtFirstLiveKeyframeWithoutCache(t *testing.T) {
	stream := newVideoStreamWithoutGOPCache(t)
	mgr := NewDASHManager("live/dash-no-cache", "/live/dash-no-cache", 6, 5)
	mgr.InitFromStream(stream)
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
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 5000, 5000, []byte{0, 0, 0, 2, 0x41, 0x03}),
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 9300, 9300, []byte{0, 0, 0, 2, 0x65, 0x04}),
	} {
		stream.WriteFrame(frame)
	}

	deadline := time.Now().Add(2 * time.Second)
	for mgr.SegmentCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	segment, ok := mgr.GetSegment(0)
	if !ok {
		t.Fatal("DASH first segment is missing")
	}
	mgr.mu.RLock()
	duration := mgr.videoSegments[0].Duration
	mgr.mu.RUnlock()
	if duration != 8.3 {
		t.Fatalf("DASH first segment duration = %.3f, want keyframe-bounded 8.3s", duration)
	}
	initData, ok := mgr.GetInitSegment()
	if !ok {
		t.Fatal("DASH video init segment is missing")
	}
	demuxer, err := fmp4.NewDemuxer(initData)
	if err != nil {
		t.Fatalf("parse DASH init segment: %v", err)
	}
	frames, err := demuxer.Parse(segment)
	if err != nil {
		t.Fatalf("parse DASH first segment: %v", err)
	}
	for _, frame := range frames {
		if frame.MediaType.IsVideo() {
			if !frame.FrameType.IsKeyframe() {
				t.Fatal("DASH advertised a first segment that starts before the first live keyframe")
			}
			return
		}
	}
	t.Fatal("DASH first segment contains no video frame")
}
