//go:build audiocodec

package push

import (
	"context"
	"fmt"
	"sync"
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
	src := source.NewFLVSourceLoop(0)
	processor, err := newWHIPAudioProcessor(src.MediaInfo().AudioCodec)
	if err != nil {
		t.Fatal(err)
	}
	defer processor.Close()
	var converted [][]byte
	for frames := 0; frames < 100 && len(converted) < 20; frames++ {
		frame, readErr := src.NextFrame()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !frame.MediaType.IsAudio() {
			continue
		}
		packets, processErr := processor.Process(frame)
		if processErr != nil {
			t.Fatal(processErr)
		}
		converted = append(converted, packets...)
	}
	if len(converted) < 20 {
		t.Fatalf("converted %d Opus packets, require at least 20", len(converted))
	}
	clock := &whipTestClock{now: time.Unix(1, 0), oversleep: 80 * time.Millisecond}
	writer := &whipTestRTPWriter{clock: clock}
	sender := whipOpusSender{track: writer, pacer: &whipRealtimePacer{enabled: true, clock: clock}}
	if sent, _, sendErr := sender.Write(context.Background(), converted); sendErr != nil || sent != int64(len(converted)) {
		t.Fatalf("sent=%d err=%v", sent, sendErr)
	}
	assertWHIPPacketEmissionTiming(t, writer.packets)
}

func TestWHIPConvertedOpusRTPTimestampsSurviveReceiverBacklog(t *testing.T) {
	capture := newWHIPRTPCapture(t)
	gate := make(chan struct{})
	capture.readGate = gate
	releaseReads := sync.OnceFunc(func() { close(gate) })
	defer releaseReads()
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

	// Queue media after the first packet while the transport remains active.
	// ReadRTP timestamps cannot establish individual sender spacing.
	packets := capture.Wait(t, 1)
	time.Sleep(150 * time.Millisecond)
	releaseReads()
	packets = append(packets, capture.Wait(t, 19)...)
	for i := 1; i < len(packets); i++ {
		duration, valid := whipOpusPacketDurationSamples(packets[i-1].Payload)
		if !valid || packets[i].Timestamp-packets[i-1].Timestamp != duration {
			t.Fatalf("converted Opus packets %d/%d have timestamps=%d/%d, prior duration=%d valid=%v", i-1, i, packets[i-1].Timestamp, packets[i].Timestamp, duration, valid)
		}
	}
	if pushErr := <-result; pushErr != nil {
		t.Fatalf("WHIP push: %v", pushErr)
	}
}
