//go:build audiocodec

package push

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/tools/testkit/source"
	"github.com/im-pingo/liveforge/tools/testkit/testutil"
)

func TestWHIPPushPublishesOpusAudio(t *testing.T) {
	srv := testutil.StartTestServer(t, testutil.WithWebRTC(), testutil.WithAudioCodec())
	pusher, err := NewPusher("whip")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, pushErr := pusher.Push(ctx, source.NewFLVSourceLoop(0), PushConfig{
			Protocol: "whip",
			Target:   fmt.Sprintf("http://%s/webrtc/whip/live/whip-audio", srv.WebRTCAddr()),
			Duration: 5 * time.Second,
			Realtime: true,
		})
		result <- pushErr
	}()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if srv.StreamHasAudio("live/whip-audio", avframe.CodecOpus) {
			cancel()
			if pushErr := <-result; pushErr != nil && pushErr != context.Canceled {
				t.Fatalf("WHIP push after audio observed: %v", pushErr)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-result
	t.Fatal("WHIP publisher never delivered an Opus audio frame")
}

func TestWHIPConvertedOpusPacketsAreIndividuallyPaced(t *testing.T) {
	capture := newWHIPRTPCapture(t)
	pusher, err := NewPusher("whip")
	if err != nil {
		t.Fatalf("NewPusher: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, pushErr := pusher.Push(ctx, source.NewFLVSourceLoop(0), PushConfig{
			Protocol: "whip",
			Target:   capture.URL(),
			Duration: 1200 * time.Millisecond,
			Realtime: true,
		})
		result <- pushErr
	}()

	packets := capture.Wait(t, 20)
	for i := 1; i < len(packets); i++ {
		if gap := packets[i].Arrival.Sub(packets[i-1].Arrival); gap < 8*time.Millisecond {
			t.Fatalf("converted Opus packets %d/%d arrived %s apart; timestamps=%d/%d durations=%v/%v; each emitted packet must be paced", i-1, i, gap, packets[i-1].Timestamp, packets[i].Timestamp, opusPacketDurationForTest(packets[i-1]), opusPacketDurationForTest(packets[i]))
		}
	}
	if pushErr := <-result; pushErr != nil {
		t.Fatalf("WHIP push: %v", pushErr)
	}
}

func opusPacketDurationForTest(packet whipCapturedRTP) time.Duration {
	samples, ok := whipOpusPacketDurationSamples(packet.Payload)
	if !ok {
		return 0
	}
	return time.Duration(samples) * time.Second / 48000
}
