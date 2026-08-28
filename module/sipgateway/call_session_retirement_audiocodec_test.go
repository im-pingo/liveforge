//go:build audiocodec

package sipgateway

import (
	"context"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

const testPCMUAnswer = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 49998 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"

func TestTranscodedOutboundCallEndsWhenPublisherGenerationRetires(t *testing.T) {
	gw, _, hub := newControlPlaneGateway(t, newTestGatewayConfig(t))
	stream, err := hub.GetOrCreate("live/transcoded-generation-retirement")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 256))
	publishTestAudio(t, stream, avframe.CodecG711A)

	dialog := &fakeInviteDialog{done: make(chan struct{})}
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
	deadline = time.Now().Add(time.Second)
	for (gw.ActiveCalls() != 0 || stream.Subscribers()["sipgateway"] != 0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := gw.ActiveCalls(); got != 0 {
		t.Fatalf("ActiveCalls after publisher retirement = %d, want 0", got)
	}
	if got := stream.Subscribers()["sipgateway"]; got != 0 {
		t.Fatalf("SIP subscribers after publisher retirement = %d, want 0", got)
	}
	dialog.mu.Lock()
	byes := dialog.byes
	dialog.mu.Unlock()
	if byes != 1 {
		t.Fatalf("BYE calls after publisher retirement = %d, want 1", byes)
	}
}
