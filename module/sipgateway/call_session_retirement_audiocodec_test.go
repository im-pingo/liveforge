//go:build audiocodec

package sipgateway

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	lfertp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/util"
)

const testPCMUAnswer = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 49998 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"

func TestTranscodedOutboundCallEndsWhenPublisherGenerationRetires(t *testing.T) {
	cfg := newTestGatewayConfig(t)
	cfg.RTPPortRange = freeSIPGatewayRTPPortRange(t, 1)
	gw, _, hub := newControlPlaneGateway(t, cfg)
	stream, err := hub.GetOrCreate("live/transcoded-generation-retirement")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 256))
	publishTestAudio(t, stream, avframe.CodecG711A)

	dialog := &fakeInviteDialog{
		done:       make(chan struct{}),
		byeStarted: make(chan struct{}, 2),
		byeRelease: make(chan struct{}),
	}
	byeReleased := false
	defer func() {
		if !byeReleased {
			close(dialog.byeRelease)
		}
	}()
	close(dialog.done)
	gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
		dialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testPCMUAnswer))
		return dialog, nil
	}

	callID, err := gw.dial(context.Background(), "alice", stream.Key(), "PCMU")
	if err != nil {
		t.Fatalf("Dial transcoded call: %v", err)
	}
	if _, ok := gw.Call(callID); !ok {
		t.Fatal("transcoded outbound call was not active")
	}
	gw.mu.RLock()
	session := gw.sessions[callID]
	gw.mu.RUnlock()
	if session == nil {
		t.Fatal("transcoded outbound session was not retained")
	}
	firstPorts := session.snapshot()
	releaseAudioReturned := make(chan struct{})
	session.mu.Lock()
	transcodedReader := session.transcodedAudio
	originalReleaseAudio := session.releaseAudio
	if originalReleaseAudio != nil {
		session.releaseAudio = func() {
			originalReleaseAudio()
			close(releaseAudioReturned)
		}
	}
	session.mu.Unlock()
	if transcodedReader == nil {
		t.Fatal("transcoded outbound session has no target-audio reader")
	}
	if originalReleaseAudio == nil {
		t.Fatal("transcoded outbound session has no target-audio release callback")
	}
	if got := stream.Subscribers()["sipgateway"]; got != 1 {
		t.Fatalf("SIP subscribers = %d, want 1 while call is active", got)
	}

	for timestamp := int64(0); timestamp < 400; timestamp += 20 {
		stream.WriteFrame(labmedia.G711Frame(avframe.CodecG711A, timestamp))
	}
	deadline := time.Now().Add(2 * time.Second)
	for gw.Metrics().RTPPacketsSent == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := gw.Metrics().RTPPacketsSent; got == 0 {
		t.Fatal("transcoded RTP did not start")
	}

	stream.RemovePublisher()
	select {
	case <-dialog.byeStarted:
	case <-time.After(time.Second):
		t.Fatal("publisher retirement did not start SIP BYE")
	}
	session.Close()
	select {
	case <-dialog.byeStarted:
		t.Fatal("late session close started a second SIP BYE")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-releaseAudioReturned:
	case <-time.After(time.Second):
		t.Fatal("target-audio release callback did not return while BYE was blocked")
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		session.mu.RLock()
		released := session.transcodedAudio == nil && session.releaseAudio == nil
		session.mu.RUnlock()
		if gw.ActiveCalls() == 0 && stream.Subscribers()["sipgateway"] == 0 && released {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := gw.ActiveCalls(); got != 0 {
		t.Fatalf("ActiveCalls after publisher retirement = %d, want 0", got)
	}
	if got := stream.Subscribers()["sipgateway"]; got != 0 {
		t.Fatalf("SIP subscribers after publisher retirement = %d, want 0", got)
	}
	session.mu.RLock()
	readerRetained := session.transcodedAudio != nil
	releaseRetained := session.releaseAudio != nil
	session.mu.RUnlock()
	if readerRetained || releaseRetained {
		t.Fatalf("transcode resources retained after publisher retirement: reader=%v release=%v", readerRetained, releaseRetained)
	}
	readCtx, cancelRead := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelRead()
	if _, ok := transcodedReader.ReadContext(readCtx); ok {
		t.Fatal("released target-audio reader still returned media")
	}
	dialog.mu.Lock()
	byesBeforeRelease := dialog.byes
	closesBeforeRelease := dialog.closes
	dialog.mu.Unlock()
	if byesBeforeRelease != 1 || closesBeforeRelease != 0 {
		t.Fatalf("dialog while BYE blocked = %d BYE/%d close, want 1/0", byesBeforeRelease, closesBeforeRelease)
	}

	if err := stream.SetPublisher(&gatewayTestPublisher{
		id: "replacement-publisher",
		info: &avframe.MediaInfo{
			AudioCodec: avframe.CodecG711A,
			SampleRate: 8000,
			Channels:   1,
		},
	}); err != nil {
		t.Fatalf("SetPublisher replacement: %v", err)
	}
	replacementDialog := &fakeInviteDialog{done: make(chan struct{})}
	close(replacementDialog.done)
	gw.sendInvite = func(_ context.Context, req *sip.Request) (inviteDialog, error) {
		replacementDialog.response = sip.NewResponseFromRequest(req, 200, "OK", []byte(testPCMUAnswer))
		return replacementDialog, nil
	}
	replacementCallID, err := gw.dial(context.Background(), "alice", stream.Key(), "PCMU")
	if err != nil {
		t.Fatalf("Dial replacement call with reclaimed pair: %v", err)
	}
	replacement, ok := gw.Call(replacementCallID)
	if !ok {
		t.Fatal("replacement call was not active")
	}
	if replacement.RTPPort != firstPorts.RTPPort || replacement.RTCPPort != firstPorts.RTCPPort {
		t.Fatalf("replacement ports = %d/%d, want reclaimed %d/%d", replacement.RTPPort, replacement.RTCPPort, firstPorts.RTPPort, firstPorts.RTCPPort)
	}
	if err := gw.Hangup(replacementCallID); err != nil {
		t.Fatalf("Hangup replacement call: %v", err)
	}

	close(dialog.byeRelease)
	byeReleased = true
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		dialog.mu.Lock()
		byes := dialog.byes
		closes := dialog.closes
		dialog.mu.Unlock()
		if byes == 1 && closes == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	dialog.mu.Lock()
	byes := dialog.byes
	closes := dialog.closes
	dialog.mu.Unlock()
	t.Fatalf("dialog after BYE release = %d BYE/%d close, want 1/1", byes, closes)
}

func TestTranscodedAudioReadyAfterPublisherRetirementDoesNotSendRTP(t *testing.T) {
	stream := core.NewStream("live/retired-transcoded-audio", config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	if err := stream.SetPublisher(&gatewayTestPublisher{
		id: "retired-audio-publisher",
		info: &avframe.MediaInfo{
			AudioCodec: avframe.CodecG711A,
			SampleRate: 8000,
			Channels:   1,
		},
	}); err != nil {
		t.Fatalf("SetPublisher: %v", err)
	}
	snapshot := stream.StartupSnapshot()
	stream.RemovePublisher()

	sourceBuffer := util.NewRingBuffer[*avframe.AVFrame](4)
	audioBuffer := util.NewRingBuffer[*avframe.AVFrame](4)
	audioBuffer.Write(labmedia.G711Frame(avframe.CodecG711U, 20))
	sourceReader := sourceBuffer.NewReader()
	audioReader := audioBuffer.NewReader()
	defer sourceReader.Close()
	defer audioReader.Close()

	audioConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen audio sender: %v", err)
	}
	defer audioConn.Close()
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen audio receiver: %v", err)
	}
	defer receiver.Close()

	packetizer, err := lfertp.NewPacketizer(avframe.CodecG711U)
	if err != nil {
		t.Fatalf("NewPacketizer: %v", err)
	}
	call := newCallSession("retired-transcoded-audio", stream.Key(), negotiatedCodec{
		Codec: avframe.CodecG711U, PT: 0, ClockRate: 8000, EncodingName: "PCMU",
	}, "outbound", audioConn.LocalAddr().(*net.UDPAddr).Port, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		call.sendTranscodedAudioAndVideo(
			ctx, stream, snapshot, sourceReader, audioReader,
			packetizer, lfertp.NewSession(0, 8000), audioConn, nil, receiver.LocalAddr().(*net.UDPAddr),
			nil, nil, nil,
		)
	}()

	buf := make([]byte, 2048)
	if err := receiver.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	n, _, readErr := receiver.ReadFromUDP(buf)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("transcoded send loop did not stop")
	}
	if readErr == nil {
		t.Fatalf("received %d bytes of retired-generation transcoded RTP", n)
	}
	if netErr, ok := readErr.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("retired-generation RTP read error = %v, want timeout", readErr)
	}
}
