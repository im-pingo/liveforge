package webrtc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/pion/interceptor"
	pionrtp "github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type whepOverwriteLogEvent struct {
	protocol    string
	reader      string
	action      string
	overwritten int64
}

type whepOverwriteLogHandler struct {
	next   slog.Handler
	events chan<- whepOverwriteLogEvent
}

func (h *whepOverwriteLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *whepOverwriteLogHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "whep: ring overwritten" {
		event := whepOverwriteLogEvent{}
		record.Attrs(func(attr slog.Attr) bool {
			switch attr.Key {
			case "protocol":
				event.protocol = attr.Value.String()
			case "reader":
				event.reader = attr.Value.String()
			case "action":
				event.action = attr.Value.String()
			case "overwritten":
				event.overwritten = attr.Value.Int64()
			}
			return true
		})
		h.events <- event
	}
	return h.next.Handle(ctx, record)
}

func (h *whepOverwriteLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &whepOverwriteLogHandler{next: h.next.WithAttrs(attrs), events: h.events}
}

func (h *whepOverwriteLogHandler) WithGroup(name string) slog.Handler {
	return &whepOverwriteLogHandler{next: h.next.WithGroup(name), events: h.events}
}

type whepCapturedRTP struct {
	header  pionrtp.Header
	payload []byte
}

type whepCapturePause struct {
	entered chan struct{}
	release chan struct{}
}

type whepRTPCapture struct {
	packets chan whepCapturedRTP
	mu      sync.Mutex
	pause   *whepCapturePause
}

func newWHEPRTPCapture() *whepRTPCapture {
	return &whepRTPCapture{packets: make(chan whepCapturedRTP, 128)}
}

func (c *whepRTPCapture) WriteRTP(header *pionrtp.Header, payload []byte) (int, error) {
	packet := whepCapturedRTP{header: *header, payload: bytes.Clone(payload)}
	c.packets <- packet
	c.mu.Lock()
	pause := c.pause
	c.pause = nil
	c.mu.Unlock()
	if pause != nil {
		close(pause.entered)
		<-pause.release
	}
	return len(payload), nil
}

func (c *whepRTPCapture) Write(raw []byte) (int, error) {
	var packet pionrtp.Packet
	if err := packet.Unmarshal(raw); err != nil {
		return 0, err
	}
	_, err := c.WriteRTP(&packet.Header, packet.Payload)
	return len(raw), err
}

func (c *whepRTPCapture) armPause() *whepCapturePause {
	c.mu.Lock()
	defer c.mu.Unlock()
	pause := &whepCapturePause{entered: make(chan struct{}), release: make(chan struct{})}
	c.pause = pause
	return pause
}

func (c *whepRTPCapture) readSample(t *testing.T) []whepCapturedRTP {
	t.Helper()
	var sample []whepCapturedRTP
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case packet := <-c.packets:
			sample = append(sample, packet)
			if packet.header.Marker {
				return sample
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for RTP sample after %d packets", len(sample))
			return nil
		}
	}
}

func (c *whepRTPCapture) assertEmpty(t *testing.T) {
	t.Helper()
	select {
	case packet := <-c.packets:
		t.Fatalf("unexpected RTP packet timestamp=%d payload=%x", packet.header.Timestamp, packet.payload)
	default:
	}
}

type whepTrackLocalContext struct {
	id     string
	codec  webrtc.RTPCodecParameters
	writer webrtc.TrackLocalWriter
}

func (c whepTrackLocalContext) ID() string                      { return c.id }
func (c whepTrackLocalContext) SSRC() webrtc.SSRC               { return 1234 }
func (c whepTrackLocalContext) SSRCRetransmission() webrtc.SSRC { return 0 }
func (c whepTrackLocalContext) SSRCForwardErrorCorrection() webrtc.SSRC {
	return 0
}
func (c whepTrackLocalContext) WriteStream() webrtc.TrackLocalWriter { return c.writer }
func (c whepTrackLocalContext) HeaderExtensions() []webrtc.RTPHeaderExtensionParameter {
	return nil
}
func (c whepTrackLocalContext) RTCPReader() interceptor.RTCPReader { return nil }
func (c whepTrackLocalContext) CodecParameters() []webrtc.RTPCodecParameters {
	return []webrtc.RTPCodecParameters{c.codec}
}

func newWHEPOverwriteSender(t *testing.T, capability webrtc.RTPCodecCapability, payloadType webrtc.PayloadType) (*TrackSender, *whepRTPCapture) {
	t.Helper()
	track, err := webrtc.NewTrackLocalStaticSample(capability, "track", "whep-overwrite")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticSample: %v", err)
	}
	capture := newWHEPRTPCapture()
	bindContext := whepTrackLocalContext{
		id: "binding",
		codec: webrtc.RTPCodecParameters{
			RTPCodecCapability: capability,
			PayloadType:        payloadType,
		},
		writer: capture,
	}
	if _, err := track.Bind(bindContext); err != nil {
		t.Fatalf("Bind track: %v", err)
	}
	t.Cleanup(func() { _ = track.Unbind(bindContext) })
	return NewTrackSender("overwrite", track, nil), capture
}

func installWHEPOverwriteLogObserver(t *testing.T) <-chan whepOverwriteLogEvent {
	t.Helper()
	events := make(chan whepOverwriteLogEvent, 16)
	previous := slog.Default()
	handler := &whepOverwriteLogHandler{
		next:   slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}),
		events: events,
	}
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return events
}

func waitWHEPOverwriteEvent(t *testing.T, events <-chan whepOverwriteLogEvent) whepOverwriteLogEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for WHEP overwrite event")
		return whepOverwriteLogEvent{}
	}
}

func waitWHEPOverwriteSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}

func whepOverwriteAVCC(nal []byte) []byte {
	payload := make([]byte, 4+len(nal))
	payload[0] = byte(len(nal) >> 24)
	payload[1] = byte(len(nal) >> 16)
	payload[2] = byte(len(nal) >> 8)
	payload[3] = byte(len(nal))
	copy(payload[4:], nal)
	return payload
}

func whepOverwriteHeader(marker byte, dts int64) *avframe.AVFrame {
	sps := whepOverwriteSPS(marker)
	pps := []byte{0x68, 0xce, 0x38, marker}
	return avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		dts, dts, buildTestAVCConfigPayload(sps, pps),
	)
}

func whepOverwriteSPS(marker byte) []byte {
	return []byte{0x67, 0x42, 0x00, 0x1f, 0xe9, marker}
}

func whepOverwriteVideo(frameType avframe.FrameType, marker byte, dts int64) *avframe.AVFrame {
	nalType := byte(0x41)
	if frameType == avframe.FrameTypeKeyframe {
		nalType = 0x65
	}
	return avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, frameType,
		dts, dts, whepOverwriteAVCC([]byte{nalType, marker}),
	)
}

func whepOverwriteAudio(marker byte, dts int64) *avframe.AVFrame {
	return avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeInterframe,
		dts, dts, []byte{marker},
	)
}

func whepSamplePayload(sample []whepCapturedRTP) []byte {
	var payload []byte
	for _, packet := range sample {
		payload = append(payload, packet.payload...)
	}
	return payload
}

func startWHEPOverwriteFeed(
	t *testing.T,
	stream *core.Stream,
	startup core.StreamStartupSnapshot,
	video, audio *TrackSender,
	mode string,
	targetAudioCodec avframe.CodecType,
	status *whepFeedStatus,
) (chan struct{}, <-chan struct{}) {
	t.Helper()
	connected := make(chan struct{})
	close(connected)
	done := make(chan struct{})
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		whepFeedLoop(stream, startup, video, audio, done, connected, mode, targetAudioCodec, nil, status)
	}()
	return done, feedDone
}

func stopWHEPOverwriteFeed(t *testing.T, done chan struct{}, feedDone <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	default:
		close(done)
	}
	select {
	case <-feedDone:
	case <-time.After(2 * time.Second):
		t.Fatal("WHEP overwrite feed did not stop")
	}
}

func TestWHEPEstablishedDirectMixedSourceOverwriteRecoversAtFreshKeyframe(t *testing.T) {
	events := installWHEPOverwriteLogObserver(t)
	stream := core.NewStream("whep/direct-mixed-overwrite", config.StreamConfig{RingBufferSize: 3, GOPCache: true, GOPCacheNum: 1}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{id: "source", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecG711A, SampleRate: 8000, Channels: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })

	oldHeader := whepOverwriteHeader(0xa0, 900)
	stream.WriteFrame(oldHeader)
	startupKeyframe := whepOverwriteVideo(avframe.FrameTypeKeyframe, 0xa1, 1000)
	startupKeyframe.PTS = 10000
	stream.WriteFrame(startupKeyframe)
	stream.WriteFrame(whepOverwriteAudio(0xa2, 1020))
	startup := stream.StartupSnapshot()
	for _, frame := range []*avframe.AVFrame{
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb0, 1100),
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb1, 1120),
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb2, 1140),
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb3, 1160),
		whepOverwriteAudio(0xb4, 1180),
	} {
		stream.WriteFrame(frame)
	}

	videoSender, videoCapture := newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, 96)
	audioSender, audioCapture := newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMA, ClockRate: 8000, Channels: 1}, 8)
	status := newWHEPFeedStatus(startup.Generation, startup.LiveCursor, "live")
	status.setExpectedMedia(true, true)
	done, feedDone := startWHEPOverwriteFeed(t, stream, startup, videoSender, audioSender, "live", avframe.CodecG711A, status)
	t.Cleanup(func() { stopWHEPOverwriteFeed(t, done, feedDone) })

	startupVideo := videoCapture.readSample(t)
	if payload := whepSamplePayload(startupVideo); !bytes.Contains(payload, whepOverwriteSPS(0xa0)) || !bytes.Contains(payload, []byte{0x65, 0xa1}) {
		t.Fatalf("startup video RTP payload = %x, want startup parameter sets and IDR", payload)
	}
	startupAudio := audioCapture.readSample(t)
	if payload := whepSamplePayload(startupAudio); !bytes.Contains(payload, []byte{0xa2}) {
		t.Fatalf("startup audio RTP payload = %x, want marker a2", payload)
	}

	if got := waitWHEPOverwriteEvent(t, events); got != (whepOverwriteLogEvent{protocol: "whep", reader: "source", action: "wait_keyframe", overwritten: 2}) {
		t.Fatalf("source overwrite event = %+v", got)
	}
	if snapshot := status.Snapshot(); snapshot.State != WHEPFeedWaitingKeyframe || snapshot.Generation != startup.Generation {
		t.Fatalf("source recovery status = %+v", snapshot)
	}
	videoCapture.assertEmpty(t)

	stream.WriteFrame(whepOverwriteAudio(0xc0, 1200))
	recoveredAudio := audioCapture.readSample(t)
	if payload := whepSamplePayload(recoveredAudio); !bytes.Contains(payload, []byte{0xc0}) {
		t.Fatalf("recovery audio RTP payload = %x, want marker c0", payload)
	}
	if delta := recoveredAudio[0].header.Timestamp - startupAudio[0].header.Timestamp; delta != 160 {
		t.Fatalf("recovered audio RTP timestamp delta = %d, want one 20ms/160 step", delta)
	}
	stream.WriteFrame(whepOverwriteAudio(0xc5, 1220))
	secondRecoveredAudio := audioCapture.readSample(t)
	if delta := secondRecoveredAudio[0].header.Timestamp - recoveredAudio[0].header.Timestamp; delta != 160 {
		t.Fatalf("post-recovery audio RTP timestamp delta = %d (%d -> %d), want one 20ms/160 step", delta, recoveredAudio[0].header.Timestamp, secondRecoveredAudio[0].header.Timestamp)
	}
	if got := status.Snapshot().State; got != WHEPFeedWaitingKeyframe {
		t.Fatalf("state after current audio only = %q, want waiting_keyframe", got)
	}

	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeInterframe, 0xc1, 1220))
	currentHeader := whepOverwriteHeader(0xc2, 1240)
	stream.WriteFrame(currentHeader)
	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeKeyframe, 0xc3, 6000))
	recoveredVideo := videoCapture.readSample(t)
	payload := whepSamplePayload(recoveredVideo)
	if !bytes.Contains(payload, whepOverwriteSPS(0xc2)) || !bytes.Contains(payload, []byte{0x65, 0xc3}) {
		t.Fatalf("first recovered video RTP payload = %x, want current parameter sets and IDR", payload)
	}
	if bytes.Contains(payload, []byte{0x41, 0xb2}) || bytes.Contains(payload, []byte{0x41, 0xc1}) {
		t.Fatalf("first recovered video RTP payload retained an interframe: %x", payload)
	}
	if delta := recoveredVideo[0].header.Timestamp - startupVideo[0].header.Timestamp; delta != 3600 {
		t.Fatalf("recovered video RTP timestamp delta = %d, want reset 40ms/3600", delta)
	}
	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeInterframe, 0xc4, 6040))
	if payload := whepSamplePayload(videoCapture.readSample(t)); !bytes.Contains(payload, []byte{0x41, 0xc4}) {
		t.Fatalf("post-recovery P-frame RTP payload = %x, want marker c4", payload)
	}
	if snapshot := status.Snapshot(); snapshot.State != WHEPFeedPlaying || snapshot.VideoFrames != 3 || snapshot.AudioFrames < 2 || snapshot.SourceOverwrites != 2 || snapshot.DroppedVideo != 1 {
		t.Fatalf("recovered mixed feed status = %+v", snapshot)
	}
}

func TestWHEPRepeatedSourceOverwriteBeforeKeyframeUsesNewestConfiguration(t *testing.T) {
	events := installWHEPOverwriteLogObserver(t)
	stream := core.NewStream("whep/repeated-source-overwrite", config.StreamConfig{RingBufferSize: 2, GOPCache: true, GOPCacheNum: 1}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{id: "source", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264, AudioCodec: avframe.CodecG711A, SampleRate: 8000, Channels: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	startupHeader := whepOverwriteHeader(0xa0, 900)
	stream.WriteFrame(startupHeader)
	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeKeyframe, 0xa1, 1000))
	stream.WriteFrame(whepOverwriteAudio(0xa2, 1020))
	startup := stream.StartupSnapshot()
	for _, frame := range []*avframe.AVFrame{
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb0, 1040),
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb1, 1060),
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb2, 1080),
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb3, 1100),
	} {
		stream.WriteFrame(frame)
	}

	videoSender, videoCapture := newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, 96)
	audioSender, audioCapture := newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMA, ClockRate: 8000, Channels: 1}, 8)
	status := newWHEPFeedStatus(startup.Generation, startup.LiveCursor, "live")
	status.setExpectedMedia(true, true)
	done, feedDone := startWHEPOverwriteFeed(t, stream, startup, videoSender, audioSender, "live", avframe.CodecG711A, status)
	t.Cleanup(func() { stopWHEPOverwriteFeed(t, done, feedDone) })
	startupVideo := videoCapture.readSample(t)
	_ = audioCapture.readSample(t)
	if got := waitWHEPOverwriteEvent(t, events); got.reader != "source" || got.overwritten != 2 {
		t.Fatalf("first overwrite event = %+v", got)
	}

	pause := audioCapture.armPause()
	stream.WriteFrame(whepOverwriteAudio(0xc0, 1120))
	waitWHEPOverwriteSignal(t, pause.entered, "established audio did not advance during first recovery")
	newestHeader := whepOverwriteHeader(0xd2, 1180)
	for _, frame := range []*avframe.AVFrame{
		whepOverwriteHeader(0xd0, 1140),
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xd1, 1160),
		newestHeader,
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xd3, 1200),
	} {
		stream.WriteFrame(frame)
	}
	close(pause.release)
	_ = audioCapture.readSample(t)
	if got := waitWHEPOverwriteEvent(t, events); got.reader != "source" || got.overwritten != 2 {
		t.Fatalf("second overwrite event = %+v", got)
	}
	videoCapture.assertEmpty(t)

	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeInterframe, 0xe0, 1220))
	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeKeyframe, 0xe1, 7000))
	recovered := videoCapture.readSample(t)
	payload := whepSamplePayload(recovered)
	if !bytes.Contains(payload, whepOverwriteSPS(0xd2)) || !bytes.Contains(payload, []byte{0x65, 0xe1}) {
		t.Fatalf("repeated-overwrite recovery RTP payload = %x, want newest retained configuration and IDR", payload)
	}
	for _, stale := range [][]byte{{0xe9, 0xa0}, {0xe9, 0xd0}, {0x41, 0xd3}, {0x41, 0xe0}} {
		if bytes.Contains(payload, stale) {
			t.Fatalf("repeated-overwrite recovery RTP payload contains stale marker %x: %x", stale, payload)
		}
	}
	if delta := recovered[0].header.Timestamp - startupVideo[0].header.Timestamp; delta != 3600 {
		t.Fatalf("repeated-overwrite RTP timestamp delta = %d, want one reset epoch", delta)
	}
	videoCapture.assertEmpty(t)
}

func TestWHEPAudioOnlyDirectSourceOverwriteContinuesAtLive(t *testing.T) {
	events := installWHEPOverwriteLogObserver(t)
	stream := core.NewStream("whep/audio-only-overwrite", config.StreamConfig{RingBufferSize: 2}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{id: "source", info: &avframe.MediaInfo{
		AudioCodec: avframe.CodecG711A, SampleRate: 8000, Channels: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	startup := stream.StartupSnapshot()
	audioSender, audioCapture := newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMA, ClockRate: 8000, Channels: 1}, 8)
	status := newWHEPFeedStatus(startup.Generation, startup.LiveCursor, "realtime")
	status.setExpectedMedia(false, true)
	done, feedDone := startWHEPOverwriteFeed(t, stream, startup, nil, audioSender, "realtime", avframe.CodecG711A, status)
	t.Cleanup(func() { stopWHEPOverwriteFeed(t, done, feedDone) })

	stream.WriteFrame(whepOverwriteAudio(0xa0, 0))
	_ = audioCapture.readSample(t)
	pause := audioCapture.armPause()
	stream.WriteFrame(whepOverwriteAudio(0xa1, 20))
	waitWHEPOverwriteSignal(t, pause.entered, "audio-only feed did not enter the deterministic write barrier")
	for _, frame := range []*avframe.AVFrame{
		whepOverwriteAudio(0xb0, 40),
		whepOverwriteAudio(0xb1, 60),
		whepOverwriteAudio(0xb2, 80),
		whepOverwriteAudio(0xb3, 100),
	} {
		stream.WriteFrame(frame)
	}
	close(pause.release)
	_ = audioCapture.readSample(t)
	if got := waitWHEPOverwriteEvent(t, events); got != (whepOverwriteLogEvent{protocol: "whep", reader: "source", action: "continue_audio", overwritten: 2}) {
		t.Fatalf("audio-only overwrite event = %+v", got)
	}
	if got := status.Snapshot().State; got == WHEPFeedWaitingKeyframe {
		t.Fatal("audio-only source overwrite entered a video keyframe wait")
	}
	audioCapture.assertEmpty(t)

	stream.WriteFrame(whepOverwriteAudio(0xc0, 120))
	payload := whepSamplePayload(audioCapture.readSample(t))
	if !bytes.Contains(payload, []byte{0xc0}) || bytes.Contains(payload, []byte{0xb2}) {
		t.Fatalf("audio-only recovered RTP payload = %x, want only next live marker c0", payload)
	}
	if got := status.Snapshot().State; got != WHEPFeedPlaying {
		t.Fatalf("audio-only recovered state = %q, want playing", got)
	}
}

func TestWHEPOverwriteRecoveryStopsBeforeReplacementGeneration(t *testing.T) {
	events := installWHEPOverwriteLogObserver(t)
	stream := core.NewStream("whep/overwrite-generation", config.StreamConfig{RingBufferSize: 2, GOPCache: true, GOPCacheNum: 1}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{id: "original", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	stream.WriteFrame(whepOverwriteHeader(0xa0, 0))
	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeKeyframe, 0xa1, 40))
	startup := stream.StartupSnapshot()
	for _, frame := range []*avframe.AVFrame{
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb0, 80),
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb1, 120),
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb2, 160),
		whepOverwriteVideo(avframe.FrameTypeInterframe, 0xb3, 200),
	} {
		stream.WriteFrame(frame)
	}
	videoSender, videoCapture := newWHEPOverwriteSender(t, webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, 96)
	status := newWHEPFeedStatus(startup.Generation, startup.LiveCursor, "live")
	status.setExpectedMedia(true, false)
	done, feedDone := startWHEPOverwriteFeed(t, stream, startup, videoSender, nil, "live", 0, status)
	_ = videoCapture.readSample(t)
	if got := waitWHEPOverwriteEvent(t, events); got.reader != "source" {
		t.Fatalf("source overwrite event = %+v", got)
	}
	videoCapture.assertEmpty(t)

	stream.RemovePublisher()
	if err := stream.SetPublisher(&authorizationTestPublisher{id: "replacement", info: &avframe.MediaInfo{VideoCodec: avframe.CodecH264}}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(whepOverwriteHeader(0xc0, 240))
	stream.WriteFrame(whepOverwriteVideo(avframe.FrameTypeKeyframe, 0xc1, 280))
	select {
	case <-feedDone:
	case <-time.After(2 * time.Second):
		select {
		case packet := <-videoCapture.packets:
			close(done)
			t.Fatalf("replacement-generation RTP escaped cancellation: timestamp=%d payload=%x", packet.header.Timestamp, packet.payload)
		default:
			close(done)
			t.Fatal("old-generation WHEP feed and reader waiters did not stop")
		}
	}
	videoCapture.assertEmpty(t)
	if snapshot := status.Snapshot(); snapshot.Generation != startup.Generation || snapshot.State != WHEPFeedGenerationEnded {
		t.Fatalf("old-generation terminal status = %+v", snapshot)
	}
	select {
	case <-done:
	default:
		close(done)
	}
}

func TestWHEPFeedClosesSendGateAtGenerationBoundary(t *testing.T) {
	stream := core.NewStream("whep/send-gate-generation", config.StreamConfig{RingBufferSize: 4}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&authorizationTestPublisher{id: "source", info: &avframe.MediaInfo{VideoCodec: avframe.CodecVP8}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	startup := stream.StartupSnapshot()
	status := newWHEPFeedStatus(startup.Generation, startup.LiveCursor, "realtime")
	status.setExpectedMedia(true, false)
	connected := make(chan struct{})
	close(connected)
	done := make(chan struct{})
	feedDone := make(chan struct{})
	gate := newWHEPSendGate()
	go func() {
		defer close(feedDone)
		whepFeedLoop(stream, startup, nil, nil, done, connected, "realtime", 0, nil, status, gate)
	}()

	stream.RemovePublisher()
	select {
	case <-feedDone:
	case <-time.After(time.Second):
		close(done)
		t.Fatal("WHEP feed did not stop after generation retirement")
	}
	if err := gate.write(func() error { return nil }); !errors.Is(err, errWHEPSendGateClosed) {
		t.Fatalf("send after generation retirement = %v, want %v", err, errWHEPSendGateClosed)
	}
	if got := status.Snapshot().State; got != WHEPFeedGenerationEnded {
		t.Fatalf("generation terminal state = %q, want generation ended", got)
	}
	close(done)
}
