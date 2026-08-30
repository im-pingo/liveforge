package gb28181

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/h264"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	pionrtp "github.com/pion/rtp/v2"
)

type gbOverwriteLogEvent struct {
	reader      string
	action      string
	overwritten int64
}

type gbOverwriteLogHandler struct {
	next   slog.Handler
	events chan<- gbOverwriteLogEvent
	pauses chan<- chan struct{}
}

func (h *gbOverwriteLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *gbOverwriteLogHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "GB28181 media reader continuity lost" {
		event := gbOverwriteLogEvent{}
		protocol := ""
		record.Attrs(func(attr slog.Attr) bool {
			switch attr.Key {
			case "protocol":
				protocol = attr.Value.String()
			case "reader":
				event.reader = attr.Value.String()
			case "recovery_action":
				event.action = attr.Value.String()
			case "overwritten":
				event.overwritten = attr.Value.Int64()
			}
			return true
		})
		if protocol == "gb28181" {
			h.events <- event
			if h.pauses != nil {
				release := make(chan struct{})
				h.pauses <- release
				<-release
			}
		}
	}
	return h.next.Handle(ctx, record)
}

func (h *gbOverwriteLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &gbOverwriteLogHandler{next: h.next.WithAttrs(attrs), events: h.events, pauses: h.pauses}
}

func (h *gbOverwriteLogHandler) WithGroup(name string) slog.Handler {
	return &gbOverwriteLogHandler{next: h.next.WithGroup(name), events: h.events, pauses: h.pauses}
}

func TestGBOutboundDirectSourceOverwriteRecoversFreshPSAtLatestHeaderAndIDR(t *testing.T) {
	events, pauses := installGBOverwriteLogObserver(t)
	stream := newGBOverwriteStream(t, "gb28181/direct-source-overwrite", 2, avframe.CodecG711A)
	stream.WriteFrame(gbOverwriteVideoHeader(0xa0, 0))
	snapshot := stream.StartupSnapshot()

	// LiveCursor is 1. A four-frame burst in a two-slot ring retains position
	// 3 while atomically reporting two overwritten positions with that value.
	for _, frame := range []*avframe.AVFrame{
		gbOverwriteAudio(0xa1, 20),
		gbOverwriteVideo(avframe.FrameTypeInterframe, 0xb1, 40),
		gbOverwriteVideo(avframe.FrameTypeInterframe, 0xb0, 60),
		gbOverwriteAudio(0xa2, 80),
	} {
		stream.WriteFrame(frame)
	}

	var muxerCreations atomic.Int32
	sender, capture, done := startGBOverwriteSender(t, stream, snapshot, func(sender *outboundMediaSession) {
		sender.newPSMuxer = func() *ps.Muxer {
			muxerCreations.Add(1)
			return ps.NewMuxer()
		}
	})
	startup := capture.readPack(t)
	assertGBPSPack(t, startup, avframe.MediaTypeVideo, 0xa0, 0)

	select {
	case got := <-events:
		want := gbOverwriteLogEvent{reader: "source", action: "wait_keyframe", overwritten: 2}
		if got != want {
			t.Fatalf("overwrite event = %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		pack := capture.readPack(t)
		t.Fatalf("no source overwrite warning; retained frame entered PS/RTP: %x", pack.raw)
	}
	close(<-pauses)

	stream.WriteFrame(gbOverwriteAudio(0xc0, 100))
	audio := capture.readPack(t)
	assertGBPSPack(t, audio, avframe.MediaTypeAudio, 0xc0, 100)

	stream.WriteFrame(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xc1, 120))
	stream.WriteFrame(gbOverwriteAudio(0xc2, 140))
	audio = capture.readPack(t)
	assertGBPSPack(t, audio, avframe.MediaTypeAudio, 0xc2, 140)

	stream.WriteFrame(gbOverwriteVideoHeader(0xc3, 160))
	stream.WriteFrame(gbOverwriteAudio(0xc4, 180))
	audio = capture.readPack(t)
	assertGBPSPack(t, audio, avframe.MediaTypeAudio, 0xc4, 180)

	stream.WriteFrame(gbOverwriteVideoHeader(0xc7, 200))
	stream.WriteFrame(gbOverwriteVideo(avframe.FrameTypeKeyframe, 0xc5, 220))
	recoveredHeader := capture.readPack(t)
	assertGBPSPack(t, recoveredHeader, avframe.MediaTypeVideo, 0xc7, 200)
	if bytes.Contains(recoveredHeader.raw, []byte{0x0c, 0xc3}) || bytes.Contains(recoveredHeader.raw, []byte{0x0c, 0xa0}) {
		t.Fatalf("recovered PS reused stale video configuration: %x", recoveredHeader.raw)
	}
	if len(recoveredHeader.raw) < 18 || binary.BigEndian.Uint32(recoveredHeader.raw[14:18]) != ps.SystemHeaderStartCode {
		t.Fatalf("recovered PS does not begin video with fresh system state: %x", recoveredHeader.raw)
	}
	recoveredIDR := capture.readPack(t)
	assertGBPSPack(t, recoveredIDR, avframe.MediaTypeVideo, 0xc5, 220)

	stream.WriteFrame(gbOverwriteVideo(avframe.FrameTypeInterframe, 0xc6, 240))
	interframe := capture.readPack(t)
	assertGBPSPack(t, interframe, avframe.MediaTypeVideo, 0xc6, 240)

	if got := capture.sequenceNumbers(); !gbRTPSequencesConsecutive(got) {
		t.Fatalf("RTP sequence numbers are not monotonic across recovery: %v", got)
	}
	if got := muxerCreations.Load(); got != 2 {
		t.Fatalf("PS muxer creations = %d, want startup plus fresh overwrite state", got)
	}
	if video, audio := sender.videoFrames.Load(), sender.audioFrames.Load(); video != 4 || audio != 3 {
		t.Fatalf("successfully sent frame counters video/audio = %d/%d, want 4/3", video, audio)
	}

	sender.cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runMedia after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GB28181 direct media loop did not stop after cancellation")
	}
}

func TestGBOutboundDirectRepeatedOverwriteBeforeIDRClearsRecoveryEpoch(t *testing.T) {
	events, pauses := installGBOverwriteLogObserver(t)
	stream := newGBOverwriteStream(t, "gb28181/direct-repeated-overwrite", 2, avframe.CodecG711A)
	stream.WriteFrame(gbOverwriteVideoHeader(0xa0, 0))
	snapshot := stream.StartupSnapshot()
	for _, frame := range []*avframe.AVFrame{
		gbOverwriteAudio(0xa1, 20),
		gbOverwriteVideo(avframe.FrameTypeInterframe, 0xa2, 40),
		gbOverwriteVideo(avframe.FrameTypeInterframe, 0xa3, 60),
		gbOverwriteAudio(0xa4, 80),
	} {
		stream.WriteFrame(frame)
	}

	var muxerCreations atomic.Int32
	sender, capture, done := startGBOverwriteSender(t, stream, snapshot, func(sender *outboundMediaSession) {
		sender.newPSMuxer = func() *ps.Muxer {
			muxerCreations.Add(1)
			return ps.NewMuxer()
		}
	})
	assertGBPSPack(t, capture.readPack(t), avframe.MediaTypeVideo, 0xa0, 0)
	assertGBOverwriteEvent(t, events, gbOverwriteLogEvent{
		reader: "source", action: "wait_keyframe", overwritten: 2,
	})
	close(<-pauses)

	stream.WriteFrame(gbOverwriteVideoHeader(0xc1, 100))
	stream.WriteFrame(gbOverwriteAudio(0xc2, 120))
	assertGBPSPack(t, capture.readPack(t), avframe.MediaTypeAudio, 0xc2, 120)
	for _, frame := range []*avframe.AVFrame{
		gbOverwriteAudio(0xd0, 140),
		gbOverwriteVideo(avframe.FrameTypeInterframe, 0xd1, 160),
		gbOverwriteVideo(avframe.FrameTypeInterframe, 0xd2, 180),
		gbOverwriteAudio(0xd3, 200),
	} {
		stream.WriteFrame(frame)
	}
	assertGBOverwriteEvent(t, events, gbOverwriteLogEvent{
		reader: "source", action: "wait_keyframe", overwritten: 2,
	})
	close(<-pauses)

	stream.WriteFrame(gbOverwriteVideo(avframe.FrameTypeKeyframe, 0xe0, 220))
	stream.WriteFrame(gbOverwriteAudio(0xe1, 240))
	assertGBPSPack(t, capture.readPack(t), avframe.MediaTypeAudio, 0xe1, 240)
	stream.WriteFrame(gbOverwriteVideoHeader(0xe2, 260))
	stream.WriteFrame(gbOverwriteVideo(avframe.FrameTypeKeyframe, 0xe3, 280))
	assertGBPSPack(t, capture.readPack(t), avframe.MediaTypeVideo, 0xe2, 260)
	assertGBPSPack(t, capture.readPack(t), avframe.MediaTypeVideo, 0xe3, 280)

	if got := muxerCreations.Load(); got != 3 {
		t.Fatalf("PS muxer creations across repeated overwrite = %d, want startup plus two fresh epochs", got)
	}
	if got := capture.sequenceNumbers(); !gbRTPSequencesConsecutive(got) {
		t.Fatalf("RTP sequence numbers are not monotonic across repeated recovery: %v", got)
	}
	sender.cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runMedia after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GB28181 repeated-overwrite media loop did not stop")
	}
}

func installGBOverwriteLogObserver(t *testing.T) (<-chan gbOverwriteLogEvent, <-chan chan struct{}) {
	t.Helper()
	events := make(chan gbOverwriteLogEvent, 16)
	pauses := make(chan chan struct{}, 16)
	previous := slog.Default()
	handler := &gbOverwriteLogHandler{
		next:   slog.NewTextHandler(gbTestLogWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelWarn}),
		events: events,
		pauses: pauses,
	}
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return events, pauses
}

func assertGBOverwriteEvent(t *testing.T, events <-chan gbOverwriteLogEvent, want gbOverwriteLogEvent) {
	t.Helper()
	select {
	case got := <-events:
		if got != want {
			t.Fatalf("overwrite event = %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for GB28181 overwrite event %+v", want)
	}
}

type gbTestLogWriter struct{ t *testing.T }

func (w gbTestLogWriter) Write(data []byte) (int, error) {
	w.t.Log(string(data))
	return len(data), nil
}

func newGBOverwriteStream(t *testing.T, key string, ringSize int, audioCodec avframe.CodecType) *core.Stream {
	t.Helper()
	stream := core.NewStream(key, config.StreamConfig{RingBufferSize: ringSize}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&gbOutboundTestPublisher{id: key + "-publisher", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: audioCodec,
		SampleRate: 8000,
		Channels:   1,
	}}); err != nil {
		t.Fatalf("SetPublisher: %v", err)
	}
	t.Cleanup(func() { stream.Close() })
	return stream
}

type gbPSCapture struct {
	conn      *net.UDPConn
	demuxer   *ps.Demuxer
	sequences []uint16
}

type gbCapturedPSPack struct {
	raw       []byte
	frames    []*avframe.AVFrame
	timestamp uint32
}

func startGBOverwriteSender(
	t *testing.T,
	stream *core.Stream,
	snapshot core.StreamStartupSnapshot,
	configure func(*outboundMediaSession),
) (*outboundMediaSession, *gbPSCapture, <-chan error) {
	t.Helper()
	sender, err := newOutboundMediaSession(stream, 0, 0)
	if err != nil {
		t.Fatalf("newOutboundMediaSession: %v", err)
	}
	sender.snapshot = snapshot
	if configure != nil {
		configure(sender)
	}
	remoteRTP, remoteRTCP, err := listenGBLabUDPPair()
	if err != nil {
		sender.close()
		t.Fatalf("listen remote media pair: %v", err)
	}
	if err := sender.setRemote(remoteRTP.LocalAddr().(*net.UDPAddr)); err != nil {
		remoteRTP.Close()
		remoteRTCP.Close()
		sender.close()
		t.Fatalf("setRemote: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- sender.runMedia() }()
	t.Cleanup(func() {
		sender.cancel()
		_ = remoteRTP.Close()
		_ = remoteRTCP.Close()
		_ = sender.rtpConn.Close()
		_ = sender.rtcpConn.Close()
	})
	return sender, &gbPSCapture{conn: remoteRTP, demuxer: ps.NewDemuxer()}, done
}

func (c *gbPSCapture) readPack(t *testing.T) gbCapturedPSPack {
	t.Helper()
	pack, err := c.readPackResult(2 * time.Second)
	if err != nil {
		t.Fatalf("read PS/RTP pack: %v", err)
	}
	return pack
}

func (c *gbPSCapture) readPackResult(timeout time.Duration) (gbCapturedPSPack, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return gbCapturedPSPack{}, err
	}
	var pack gbCapturedPSPack
	for {
		buf := make([]byte, 2048)
		n, _, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			return gbCapturedPSPack{}, err
		}
		var packet pionrtp.Packet
		if unmarshalErr := packet.Unmarshal(buf[:n]); unmarshalErr != nil {
			return gbCapturedPSPack{}, unmarshalErr
		}
		if len(pack.raw) == 0 {
			pack.timestamp = packet.Timestamp
		} else if pack.timestamp != packet.Timestamp {
			return gbCapturedPSPack{}, fmt.Errorf("RTP timestamp changed within PS pack: %d then %d", pack.timestamp, packet.Timestamp)
		}
		c.sequences = append(c.sequences, packet.SequenceNumber)
		pack.raw = append(pack.raw, packet.Payload...)
		if !packet.Marker {
			continue
		}
		pack.frames, err = c.demuxer.Feed(pack.raw)
		return pack, err
	}
}

func (c *gbPSCapture) sequenceNumbers() []uint16 {
	return slices.Clone(c.sequences)
}

func assertGBPSPack(t *testing.T, pack gbCapturedPSPack, mediaType avframe.MediaType, marker byte, dts int64) {
	t.Helper()
	if pack.timestamp != uint32(dts*90) { // #nosec G115 -- test timestamps are bounded fixtures.
		t.Fatalf("PS/RTP timestamp = %d, want unchanged DTS timestamp %d", pack.timestamp, dts*90)
	}
	if !bytes.Contains(pack.raw, []byte{marker}) {
		t.Fatalf("PS pack does not contain marker %02x: %x", marker, pack.raw)
	}
	for _, frame := range pack.frames {
		if frame != nil && frame.MediaType == mediaType {
			return
		}
	}
	t.Fatalf("real PS demux returned no media type %v frame for marker %02x: %#v", mediaType, marker, pack.frames)
}

func gbRTPSequencesConsecutive(sequences []uint16) bool {
	if len(sequences) == 0 {
		return false
	}
	for i := 1; i < len(sequences); i++ {
		if sequences[i] != sequences[i-1]+1 {
			return false
		}
	}
	return true
}

func gbOverwriteAudio(marker byte, dts int64) *avframe.AVFrame {
	return avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeInterframe,
		dts, dts, []byte{marker},
	)
}

func gbOverwriteVideo(frameType avframe.FrameType, marker byte, dts int64) *avframe.AVFrame {
	nalType := byte(0x41)
	if frameType == avframe.FrameTypeKeyframe {
		nalType = 0x65
	}
	return avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, frameType,
		dts, dts, []byte{0, 0, 0, 1, nalType, marker},
	)
}

func gbOverwriteVideoHeader(marker byte, dts int64) *avframe.AVFrame {
	var payload []byte
	for _, nalu := range h264.ExtractNALUs(labmedia.VideoFrame(0).Payload) {
		if len(nalu) == 0 || (nalu[0]&0x1f != 7 && nalu[0]&0x1f != 8) {
			continue
		}
		payload = append(payload, 0, 0, 0, 1)
		payload = append(payload, nalu...)
	}
	payload = append(payload, 0, 0, 0, 1, 0x0c, marker)
	return avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		dts, dts, payload,
	)
}
