package sipgateway

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pionrtp "github.com/pion/rtp/v2"
)

type sipOverwriteLogEvent struct {
	reader      string
	action      string
	overwritten int64
}

type sipOverwriteLogHandler struct {
	next   slog.Handler
	events chan<- sipOverwriteLogEvent
	pauses chan<- chan struct{}
}

func (h *sipOverwriteLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *sipOverwriteLogHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "SIP media reader continuity lost" {
		event := sipOverwriteLogEvent{}
		protocol := ""
		record.Attrs(func(attr slog.Attr) bool {
			switch attr.Key {
			case "protocol":
				protocol = attr.Value.String()
			case "media_reader":
				event.reader = attr.Value.String()
			case "recovery_action":
				event.action = attr.Value.String()
			case "overwritten":
				event.overwritten = attr.Value.Int64()
			}
			return true
		})
		if protocol == "sip" {
			select {
			case h.events <- event:
			default:
			}
			if h.pauses != nil {
				release := make(chan struct{})
				h.pauses <- release
				<-release
			}
		}
	}
	return h.next.Handle(ctx, record)
}

func (h *sipOverwriteLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &sipOverwriteLogHandler{next: h.next.WithAttrs(attrs), events: h.events, pauses: h.pauses}
}

func (h *sipOverwriteLogHandler) WithGroup(name string) slog.Handler {
	return &sipOverwriteLogHandler{next: h.next.WithGroup(name), events: h.events, pauses: h.pauses}
}

func TestCallSessionOutboundDirectMixedSourceOverwriteRecoversAudioAndVideo(t *testing.T) {
	events := installSIPOverwriteLogObserver(t)
	stream := newSIPOverwriteStream(t, "sip/direct-mixed-overwrite", 2, true)
	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeSequenceHeader, 0xa0))
	snapshot := stream.StartupSnapshot()

	// The captured cursor is 1. Four writes into a two-slot ring retain position
	// 3 and atomically report two overwritten positions on the first read.
	stream.WriteFrame(sipAudioFrame(0xa1))
	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeInterframe, 0xb1))
	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeInterframe, 0xb0))
	stream.WriteFrame(sipAudioFrame(0xa2))

	call, remoteAudio, remoteVideo := startSIPOverwriteCall(t, stream, snapshot, true)
	assertSIPOverwriteEvent(t, events, sipOverwriteLogEvent{
		reader: "source", action: "wait_keyframe", overwritten: 2,
	})
	assertRTPMarkers(t, remoteVideo, []byte{0xa0})

	stream.WriteFrame(sipAudioFrame(0xc0))
	assertRTPMarkers(t, remoteAudio, []byte{0xc0})
	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeInterframe, 0xc1))
	stream.WriteFrame(sipAudioFrame(0xc5))
	assertRTPMarkers(t, remoteAudio, []byte{0xc5})
	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeSequenceHeader, 0xc2))
	stream.WriteFrame(sipAudioFrame(0xc6))
	assertRTPMarkers(t, remoteAudio, []byte{0xc6})
	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeSequenceHeader, 0xc7))
	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeKeyframe, 0xc3))
	assertRTPMarkers(t, remoteVideo, []byte{0xc7, 0xc3})
	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeInterframe, 0xc4))
	assertRTPMarkers(t, remoteVideo, []byte{0xc4})
	call.Close()
	if got := call.rtpPacketsSent.Load(); got != 7 {
		t.Fatalf("RTP packets sent = %d, want only startup header, live audio, refreshed header, IDR, and later video", got)
	}
}

func TestCallSessionOutboundDirectAudioOnlySourceOverwriteContinues(t *testing.T) {
	events := installSIPOverwriteLogObserver(t)
	stream := newSIPOverwriteStream(t, "sip/direct-audio-overwrite", 2, false)
	snapshot := stream.StartupSnapshot()

	for _, marker := range []byte{0xa1, 0xa2, 0xa3, 0xa4} {
		stream.WriteFrame(sipAudioFrame(marker))
	}

	call, remoteAudio, _ := startSIPOverwriteCall(t, stream, snapshot, false)
	assertSIPOverwriteEvent(t, events, sipOverwriteLogEvent{
		reader: "source", action: "continue_audio", overwritten: 2,
	})
	stream.WriteFrame(sipAudioFrame(0xb0))

	assertRTPMarkers(t, remoteAudio, []byte{0xb0})
	call.Close()
	if got := call.rtpPacketsSent.Load(); got != 1 {
		t.Fatalf("RTP packets sent = %d, want only the next live audio packet", got)
	}
}

func TestCallSessionOutboundRepeatedSourceOverwriteBeforeIDRRestartsVideoGate(t *testing.T) {
	events, pauses := installPausedSIPOverwriteLogObserver(t)
	stream := newSIPOverwriteStream(t, "sip/repeated-source-overwrite", 2, true)
	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeSequenceHeader, 0xa0))
	snapshot := stream.StartupSnapshot()
	for _, frame := range []*avframe.AVFrame{
		sipAudioFrame(0xa1),
		sipVideoFrame(avframe.FrameTypeInterframe, 0xb1),
		sipVideoFrame(avframe.FrameTypeInterframe, 0xb0),
		sipAudioFrame(0xa2),
	} {
		stream.WriteFrame(frame)
	}

	call, remoteAudio, remoteVideo := startSIPOverwriteCall(t, stream, snapshot, true)
	assertSIPOverwriteEvent(t, events, sipOverwriteLogEvent{
		reader: "source", action: "wait_keyframe", overwritten: 2,
	})
	close(<-pauses)
	assertRTPMarkers(t, remoteVideo, []byte{0xa0})

	// The audio marker is an ordered barrier proving the first recovery header
	// was observed and retained before the second overwrite is induced.
	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeSequenceHeader, 0xc1))
	stream.WriteFrame(sipAudioFrame(0xc2))
	readRTPUntilMarker(t, remoteAudio, 0xc2)

	// Audio RTP makes this reader slower than the writer. With a two-slot ring,
	// this bounded burst deterministically creates another positive overwrite.
	for i := range 64 {
		stream.WriteFrame(sipAudioFrame(byte(i)))
	}
	second := waitSIPOverwriteEvent(t, events)
	if second.reader != "source" || second.action != "wait_keyframe" || second.overwritten <= 0 {
		t.Fatalf("second overwrite event = %+v, want positive source wait_keyframe", second)
	}
	secondRelease := <-pauses
	for _, marker := range []byte{0xe1, 0xe2, 0xe3, 0xe4} {
		stream.WriteFrame(sipAudioFrame(marker))
	}
	close(secondRelease)
	third := waitSIPOverwriteEvent(t, events)
	if third.reader != "source" || third.action != "wait_keyframe" || third.overwritten <= 0 {
		t.Fatalf("third overwrite event = %+v, want positive source wait_keyframe", third)
	}
	close(<-pauses)

	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeKeyframe, 0xd0))
	stream.WriteFrame(sipAudioFrame(0xfe))
	readRTPUntilMarker(t, remoteAudio, 0xfe)
	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeSequenceHeader, 0xd1))
	stream.WriteFrame(sipAudioFrame(0xfd))
	readRTPUntilMarker(t, remoteAudio, 0xfd)
	stream.WriteFrame(sipVideoFrame(avframe.FrameTypeKeyframe, 0xd2))
	assertRTPMarkers(t, remoteVideo, []byte{0xd1, 0xd2})

	call.Close()
}

func installSIPOverwriteLogObserver(t *testing.T) <-chan sipOverwriteLogEvent {
	t.Helper()
	events, _ := installSIPOverwriteLogObserverWithPauses(t, false)
	return events
}

func installPausedSIPOverwriteLogObserver(t *testing.T) (<-chan sipOverwriteLogEvent, <-chan chan struct{}) {
	t.Helper()
	return installSIPOverwriteLogObserverWithPauses(t, true)
}

func installSIPOverwriteLogObserverWithPauses(t *testing.T, pause bool) (<-chan sipOverwriteLogEvent, <-chan chan struct{}) {
	t.Helper()
	events := make(chan sipOverwriteLogEvent, 16)
	var pauses chan chan struct{}
	if pause {
		pauses = make(chan chan struct{}, 16)
	}
	previous := slog.Default()
	handler := &sipOverwriteLogHandler{
		next:   slog.NewTextHandler(testLogWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelWarn}),
		events: events,
		pauses: pauses,
	}
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return events, pauses
}

type testLogWriter struct {
	t *testing.T
}

func (w testLogWriter) Write(data []byte) (int, error) {
	w.t.Log(string(data))
	return len(data), nil
}

func assertSIPOverwriteEvent(t *testing.T, events <-chan sipOverwriteLogEvent, want sipOverwriteLogEvent) {
	t.Helper()
	if got := waitSIPOverwriteEvent(t, events); got != want {
		t.Fatalf("overwrite event = %+v, want %+v", got, want)
	}
}

func waitSIPOverwriteEvent(t *testing.T, events <-chan sipOverwriteLogEvent) sipOverwriteLogEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SIP overwrite warning")
		return sipOverwriteLogEvent{}
	}
}

func newSIPOverwriteStream(t *testing.T, key string, ringSize int, withVideo bool) *core.Stream {
	t.Helper()
	stream := core.NewStream(key, config.StreamConfig{RingBufferSize: ringSize}, config.LimitsConfig{}, core.NewEventBus())
	info := &avframe.MediaInfo{AudioCodec: avframe.CodecG711A, SampleRate: 8000, Channels: 1}
	if withVideo {
		info.VideoCodec = avframe.CodecH264
	}
	if err := stream.SetPublisher(&gatewayTestPublisher{id: key + "-publisher", info: info}); err != nil {
		t.Fatalf("SetPublisher: %v", err)
	}
	t.Cleanup(func() { stream.Close() })
	return stream
}

func startSIPOverwriteCall(
	t *testing.T,
	stream *core.Stream,
	snapshot core.StreamStartupSnapshot,
	withVideo bool,
) (*CallSession, *net.UDPConn, *net.UDPConn) {
	t.Helper()
	remoteAudioRTP, remoteAudioRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("listen remote audio pair: %v", err)
	}
	t.Cleanup(func() {
		_ = remoteAudioRTP.Close()
		_ = remoteAudioRTCP.Close()
	})
	localAudioRTP, localAudioRTCP, err := listenLabUDPPair()
	if err != nil {
		t.Fatalf("listen local audio pair: %v", err)
	}

	call := newCallSession(
		"overwrite-"+stream.Key(), stream.Key(),
		negotiatedCodec{Codec: avframe.CodecG711A, PT: 8, ClockRate: 8000, EncodingName: "PCMA"},
		"outbound",
		localAudioRTP.LocalAddr().(*net.UDPAddr).Port,
		localAudioRTCP.LocalAddr().(*net.UDPAddr).Port,
	)
	call.configureMediaSockets(localAudioRTP, localAudioRTCP)

	var remoteVideoRTP *net.UDPConn
	if withVideo {
		var remoteVideoRTCP *net.UDPConn
		remoteVideoRTP, remoteVideoRTCP, err = listenLabUDPPair()
		if err != nil {
			t.Fatalf("listen remote video pair: %v", err)
		}
		t.Cleanup(func() {
			_ = remoteVideoRTP.Close()
			_ = remoteVideoRTCP.Close()
		})
		localVideoRTP, localVideoRTCP, listenErr := listenLabUDPPair()
		if listenErr != nil {
			t.Fatalf("listen local video pair: %v", listenErr)
		}
		call.configureVideo(
			sipH264Codec,
			localVideoRTP.LocalAddr().(*net.UDPAddr).Port,
			localVideoRTCP.LocalAddr().(*net.UDPAddr).Port,
			"127.0.0.1",
			remoteVideoRTP.LocalAddr().(*net.UDPAddr).Port,
		)
		call.configureVideoSockets(localVideoRTP, localVideoRTCP)
	}
	t.Cleanup(call.Close)
	if err := call.startOutbound(stream, snapshot, "127.0.0.1", remoteAudioRTP.LocalAddr().(*net.UDPAddr).Port); err != nil {
		t.Fatalf("startOutbound: %v", err)
	}
	return call, remoteAudioRTP, remoteVideoRTP
}

func sipAudioFrame(marker byte) *avframe.AVFrame {
	return avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecG711A, avframe.FrameTypeInterframe, 0, 0, []byte{marker})
}

func sipVideoFrame(frameType avframe.FrameType, marker byte) *avframe.AVFrame {
	nalType := byte(0x41)
	switch frameType {
	case avframe.FrameTypeSequenceHeader:
		nalType = 0x67
	case avframe.FrameTypeKeyframe:
		nalType = 0x65
	}
	return avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, frameType, 0, 0, []byte{nalType, marker})
}

func assertRTPMarkers(t *testing.T, conn *net.UDPConn, want []byte) {
	t.Helper()
	got := make([]byte, 0, len(want))
	for len(got) < len(want) {
		got = append(got, readRTPMarker(t, conn))
	}
	if string(got) != string(want) {
		t.Fatalf("RTP markers = %x, want %x", got, want)
	}
}

func readRTPUntilMarker(t *testing.T, conn *net.UDPConn, want byte) {
	t.Helper()
	for attempts := 0; attempts < 1024; attempts++ {
		if got := readRTPMarker(t, conn); got == want {
			return
		}
	}
	t.Fatalf("RTP marker %02x not observed", want)
}

func readRTPMarker(t *testing.T, conn *net.UDPConn) byte {
	t.Helper()
	if conn == nil {
		t.Fatal("nil RTP receiver")
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 2048)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("ReadFromUDP: %v", err)
	}
	var packet pionrtp.Packet
	if err := packet.Unmarshal(buf[:n]); err != nil {
		t.Fatalf("Unmarshal RTP: %v", err)
	}
	if len(packet.Payload) == 0 {
		t.Fatal("RTP packet has empty payload")
	}
	return packet.Payload[len(packet.Payload)-1]
}
