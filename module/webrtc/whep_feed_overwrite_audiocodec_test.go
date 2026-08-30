//go:build audiocodec

package webrtc

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/util"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	whepOverwriteSourceAudio avframe.CodecType = 240
	whepOverwriteTargetAudio avframe.CodecType = 241
)

type whepOverwriteDecoder struct {
	blockMarker byte
	entered     chan struct{}
	release     chan struct{}
	decoded     chan byte
	blockOnce   sync.Once
}

func (*whepOverwriteDecoder) SetExtradata([]byte) {}

func (d *whepOverwriteDecoder) Decode(payload []byte) (*audiocodec.PCMFrame, error) {
	marker := byte(0)
	if len(payload) > 0 {
		marker = payload[0]
	}
	if d.entered != nil && marker == d.blockMarker {
		d.blockOnce.Do(func() { close(d.entered) })
		<-d.release
	}
	if d.decoded != nil {
		d.decoded <- marker
	}
	return &audiocodec.PCMFrame{Samples: []int16{int16(marker)}, SampleRate: 48000, Channels: 1}, nil
}

func (*whepOverwriteDecoder) SampleRate() int { return 48000 }
func (*whepOverwriteDecoder) Channels() int   { return 1 }
func (*whepOverwriteDecoder) Close()          {}

type whepOverwriteEncoder struct{}

func (*whepOverwriteEncoder) Encode(pcm *audiocodec.PCMFrame) ([]byte, error) {
	if pcm == nil || len(pcm.Samples) == 0 {
		return nil, nil
	}
	return []byte{byte(pcm.Samples[0])}, nil // #nosec G115 -- the encoder intentionally emits a one-byte test marker.
}

func (*whepOverwriteEncoder) SampleRate() int { return 48000 }
func (*whepOverwriteEncoder) Channels() int   { return 1 }
func (*whepOverwriteEncoder) FrameSize() int  { return 1 }
func (*whepOverwriteEncoder) Close()          {}

func newWHEPOverwriteTranscodeManager(stream *core.Stream, bufferSize int, decoder *whepOverwriteDecoder) {
	registry := audiocodec.Global()
	registry.RegisterDecoder(whepOverwriteSourceAudio, func() audiocodec.Decoder {
		if decoder != nil {
			return decoder
		}
		return &whepOverwriteDecoder{}
	})
	registry.RegisterEncoder(whepOverwriteTargetAudio, func() audiocodec.Encoder { return &whepOverwriteEncoder{} })
	manager := core.NewTranscodeManager(stream, registry, bufferSize)
	core.SetTranscodeManagerForTest(stream, manager)
}

func whepOverwriteTranscodeAudio(marker byte, dts int64) *avframe.AVFrame {
	return avframe.NewAVFrame(
		avframe.MediaTypeAudio, whepOverwriteSourceAudio, avframe.FrameTypeInterframe,
		dts, dts, []byte{marker},
	)
}

func waitWHEPDecodedMarker(t *testing.T, decoded <-chan byte, target byte) {
	t.Helper()
	select {
	case got := <-decoded:
		if got != target {
			t.Fatalf("decoded source-audio marker = %x, want %x", got, target)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("target-audio producer did not decode source marker %x", target)
	}
}

func drainWHEPCapture(capture *whepRTPCapture) {
	for {
		select {
		case <-capture.packets:
		default:
			return
		}
	}
}

func TestWHEPTranscodedMixedSourceOverwriteKeepsTargetAudioAndRecoversVideo(t *testing.T) {
	events := installWHEPOverwriteLogObserver(t)
	stream := core.NewStream("whep/transcoded-source-overwrite", config.StreamConfig{
		RingBufferSize: 3, GOPCache: true, GOPCacheNum: 1,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{id: "source", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264, AudioCodec: whepOverwriteSourceAudio, SampleRate: 48000, Channels: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	decoder := &whepOverwriteDecoder{decoded: make(chan byte, 16)}
	newWHEPOverwriteTranscodeManager(stream, 64, decoder)
	stream.WriteFrame(whepOverwriteHeader(0xa0, 900))
	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeKeyframe, 0xa1, 1000))
	stream.WriteFrame(whepOverwriteTranscodeAudio(0xa2, 1020))
	startup := stream.StartupSnapshot()

	videoSender, videoCapture := newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, 96)
	audioSender, audioCapture := newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 1}, 111)
	status := newWHEPFeedStatus(startup.Generation, startup.LiveCursor, "live")
	status.setExpectedMedia(true, true)
	done, feedDone := startWHEPOverwriteFeed(t, stream, startup, videoSender, audioSender, "live", whepOverwriteTargetAudio, status)
	t.Cleanup(func() { stopWHEPOverwriteFeed(t, done, feedDone) })
	startupVideo := videoCapture.readSample(t)
	if payload := whepSamplePayload(audioCapture.readSample(t)); !bytes.Contains(payload, []byte{0xa2}) {
		t.Fatalf("startup target-audio RTP payload = %x, want marker a2", payload)
	}
	waitWHEPDecodedMarker(t, decoder.decoded, 0xa2)

	pause := videoCapture.armPause()
	releasePause := sync.OnceFunc(func() { close(pause.release) })
	t.Cleanup(releasePause)
	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb0, 1040))
	waitWHEPOverwriteSignal(t, pause.entered, "source video did not enter deterministic WHEP write barrier")
	stream.WriteFrame(whepOverwriteTranscodeAudio(0xb1, 1060))
	waitWHEPDecodedMarker(t, decoder.decoded, 0xb1)
	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb2, 1080))
	stream.WriteFrame(whepOverwriteTranscodeAudio(0xb3, 1100))
	waitWHEPDecodedMarker(t, decoder.decoded, 0xb3)
	for _, frame := range []*avframe.AVFrame{
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb4, 1120),
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb5, 1130),
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb6, 1140),
	} {
		stream.WriteFrame(frame)
	}
	releasePause()
	_ = videoCapture.readSample(t)
	for _, marker := range []byte{0xb1, 0xb3} {
		if payload := whepSamplePayload(audioCapture.readSample(t)); !bytes.Contains(payload, []byte{marker}) {
			t.Fatalf("target audio during source recovery = %x, want marker %x", payload, marker)
		}
	}
	if got := waitWHEPOverwriteEvent(t, events); got.protocol != "whep" || got.reader != "source" || got.action != "wait_keyframe" || got.overwritten <= 0 {
		t.Fatalf("transcoded source overwrite event = %+v", got)
	}
	if got := status.Snapshot().State; got != WHEPFeedWaitingKeyframe {
		t.Fatalf("transcoded source recovery state = %q, want waiting_keyframe", got)
	}

	stream.WriteFrame(whepOverwriteTranscodeAudio(0xc0, 1140))
	waitWHEPDecodedMarker(t, decoder.decoded, 0xc0)
	if payload := whepSamplePayload(audioCapture.readSample(t)); !bytes.Contains(payload, []byte{0xc0}) {
		t.Fatalf("post-overwrite target audio RTP payload = %x, want marker c0", payload)
	}
	if got := status.Snapshot().State; got != WHEPFeedWaitingKeyframe {
		t.Fatalf("target audio alone cleared video recovery state: %q", got)
	}
	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeInterframe, 0xc1, 1160))
	stream.WriteFrame(whepOverwriteHeader(0xc2, 1180))
	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeKeyframe, 0xc3, 6000))
	recovered := videoCapture.readSample(t)
	payload := whepSamplePayload(recovered)
	if !bytes.Contains(payload, whepOverwriteSPS(0xc2)) || !bytes.Contains(payload, []byte{0x65, 0xc3}) || bytes.Contains(payload, []byte{0x41, 0xc1}) {
		t.Fatalf("transcoded first recovered video RTP payload = %x", payload)
	}
	if delta := recovered[0].header.Timestamp - startupVideo[0].header.Timestamp; delta != 7200 {
		// One clean interframe was admitted before the overwrite barrier; recovery
		// itself must add one normal 40ms step rather than the source DTS gap.
		t.Fatalf("transcoded recovered RTP timestamp delta = %d, want two 40ms steps/7200", delta)
	}
}

func TestWHEPTargetAudioOverwriteKeepsDirectVideoContinuous(t *testing.T) {
	events := installWHEPOverwriteLogObserver(t)
	stream := core.NewStream("whep/target-audio-overwrite", config.StreamConfig{RingBufferSize: 8}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{id: "source", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecVP8, AudioCodec: whepOverwriteSourceAudio,
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	snapshot := stream.StartupSnapshot()
	sourceRing := util.NewRingBuffer[*avframe.AVFrame](8)
	targetRing := util.NewRingBuffer[*avframe.AVFrame](2)
	sourceReader := sourceRing.NewReaderAt(0)
	targetReader := targetRing.NewReaderAt(0)
	readers := &whepFeedReaders{targetAudio: targetReader}
	t.Cleanup(readers.Close)
	for _, frame := range []*avframe.AVFrame{
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeInterframe, 20, 20, []byte{0xb0}),
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecVP8, avframe.FrameTypeInterframe, 40, 40, []byte{0xb1}),
	} {
		sourceRing.Write(frame)
	}
	for _, marker := range []byte{0xa0, 0xa1, 0xa2, 0xa3} {
		targetRing.Write(avframe.NewAVFrame(avframe.MediaTypeAudio, whepOverwriteTargetAudio, avframe.FrameTypeInterframe, int64(marker), int64(marker), []byte{marker}))
	}
	readers.startWaiters(nil, nil)
	if !readers.wait(nil, nil) {
		t.Fatal("target-audio pump did not publish the full-ring overwrite")
	}

	audioSender, audioCapture := newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 1}, 111)
	writeAudio := func(frame *avframe.AVFrame) bool {
		return audioSender.WriteSample(mediaSample(frame.Payload, 20*time.Millisecond)) == nil
	}
	if !readers.drainTargetAudio(stream, snapshot.Generation, true, whepOverwriteTargetAudio, writeAudio, nil) {
		t.Fatal("target-audio overwrite stopped the active generation")
	}
	if got := waitWHEPOverwriteEvent(t, events); got != (whepOverwriteLogEvent{protocol: "whep", reader: "target_audio", action: "continue_audio", overwritten: 2}) {
		t.Fatalf("target-audio overwrite event = %+v", got)
	}
	if got := sourceReader.ReadCursor(); got != 0 {
		t.Fatalf("target-audio overwrite advanced source reader to %d, want 0", got)
	}
	audioCapture.assertEmpty(t)
	targetRing.Write(avframe.NewAVFrame(avframe.MediaTypeAudio, whepOverwriteTargetAudio, avframe.FrameTypeInterframe, 120, 120, []byte{0xc0}))
	if !readers.wait(nil, nil) {
		t.Fatal("target-audio pump did not publish the post-overwrite frame")
	}
	if !readers.drainTargetAudio(stream, snapshot.Generation, true, whepOverwriteTargetAudio, writeAudio, nil) {
		t.Fatal("target audio did not continue from live")
	}
	if payload := whepSamplePayload(audioCapture.readSample(t)); !bytes.Contains(payload, []byte{0xc0}) {
		t.Fatalf("target-audio recovered RTP payload = %x, want c0", payload)
	}

	videoSender, videoCapture := newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, 96)
	for _, marker := range []byte{0xb0, 0xb1} {
		result := sourceReader.TryReadResult()
		if !result.OK || result.Overwritten != 0 {
			t.Fatalf("clean source read after target overwrite = %+v", result)
		}
		if err := videoSender.WriteSample(mediaSample(result.Value.Payload, 40*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if payload := whepSamplePayload(videoCapture.readSample(t)); !bytes.Contains(payload, []byte{marker}) {
			t.Fatalf("continuous video RTP payload = %x, want marker %x", payload, marker)
		}
	}
}

func TestWHEPTranscodeProducerSourceOverwriteEOFIsTerminalAndReleasesOnce(t *testing.T) {
	decoder := &whepOverwriteDecoder{blockMarker: 0xe0, entered: make(chan struct{}), release: make(chan struct{})}
	stream := core.NewStream("whep/transcode-producer-overwrite", config.StreamConfig{
		RingBufferSize: 4, GOPCache: true, GOPCacheNum: 1,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{id: "source", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264, AudioCodec: whepOverwriteSourceAudio, SampleRate: 48000, Channels: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	newWHEPOverwriteTranscodeManager(stream, 16, decoder)
	stream.WriteFrame(whepOverwriteHeader(0xa0, 0))
	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeKeyframe, 0xa1, 40))
	stream.WriteFrame(whepOverwriteTranscodeAudio(0xa2, 60))
	startup := stream.StartupSnapshot()
	videoSender, videoCapture := newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, 96)
	audioSender, audioCapture := newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 1}, 111)
	status := newWHEPFeedStatus(startup.Generation, startup.LiveCursor, "live")
	status.setExpectedMedia(true, true)
	done, feedDone := startWHEPOverwriteFeed(t, stream, startup, videoSender, audioSender, "live", whepOverwriteTargetAudio, status)
	_ = videoCapture.readSample(t)
	_ = audioCapture.readSample(t)

	stream.WriteFrame(whepOverwriteTranscodeAudio(0xe0, 80))
	waitWHEPOverwriteSignal(t, decoder.entered, "target-audio producer did not enter decoder barrier")
	pumpStop := make(chan struct{})
	pumpDone := make(chan struct{})
	pumped := make(chan struct{}, 8)
	stopPump := sync.OnceFunc(func() { close(pumpStop) })
	t.Cleanup(func() {
		stopPump()
		<-pumpDone
	})
	go func() {
		defer close(pumpDone)
		nextIndex := 0
		writeNext := func() {
			dts := int64(100 + nextIndex*20)
			stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeInterframe, byte(nextIndex), dts)) // #nosec G115 -- overwrite fixture index is bounded.
			nextIndex++
		}
		writeNext()
		writeNext()
		for {
			select {
			case <-pumpStop:
				return
			case packet := <-videoCapture.packets:
				if packet.header.Marker {
					writeNext()
					select {
					case pumped <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	for range 6 {
		waitWHEPOverwriteSignal(t, pumped, "continuous source video did not advance while target producer was blocked")
	}
	close(decoder.release)
	select {
	case <-feedDone:
	case <-time.After(2 * time.Second):
		stopPump()
		close(done)
		t.Fatal("active target-audio EOF waited for the WHEP stall watchdog")
	}
	stopPump()
	<-pumpDone
	if snapshot := status.Snapshot(); snapshot.State != WHEPFeedTargetAudioFailed || !bytes.Contains([]byte(snapshot.LastError), []byte("target audio")) {
		t.Fatalf("target-audio EOF terminal status = %+v", snapshot)
	}
	drainWHEPCapture(videoCapture)
	drainWHEPCapture(audioCapture)
	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeKeyframe, 0xc0, 180))
	stream.WriteFrame(whepOverwriteTranscodeAudio(0xc1, 200))
	videoCapture.assertEmpty(t)
	audioCapture.assertEmpty(t)

	var releases atomic.Int32
	owned := &whepFeedReaders{
		source:      util.NewRingBuffer[*avframe.AVFrame](1).NewReader(),
		targetAudio: util.NewRingBuffer[*avframe.AVFrame](1).NewReader(),
		release:     func() { releases.Add(1) },
	}
	owned.Close()
	owned.Close()
	if got := releases.Load(); got != 1 {
		t.Fatalf("target-audio release calls = %d, want 1", got)
	}
}

func mediaSample(payload []byte, duration time.Duration) media.Sample {
	return media.Sample{Data: payload, Duration: duration}
}
