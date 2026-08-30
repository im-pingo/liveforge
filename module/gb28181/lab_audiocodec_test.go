//go:build audiocodec

package gb28181

import (
	"context"
	"testing"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestGBLabReceiveTranscodesOpusToG711A(t *testing.T) {
	h := newRealGBLabHarness(t)
	stream, err := h.hub.GetOrCreate("gb28181/receive-opus-to-g711a")
	if err != nil {
		t.Fatalf("GetOrCreate receive stream: %v", err)
	}
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 256))
	if publisherErr := stream.SetPublisher(&gbOutboundTestPublisher{id: "gb-lab-opus-source", info: &avframe.MediaInfo{
		VideoCodec:          avframe.CodecH264,
		VideoSequenceHeader: labmedia.VideoFrame(0).Payload,
		AudioCodec:          avframe.CodecOpus,
		SampleRate:          48000,
		Channels:            2,
	}}); publisherErr != nil {
		t.Fatalf("SetPublisher receive source: %v", publisherErr)
	}

	encoder := audiocodec.NewFFmpegEncoder("libopus", 48000, 2)
	defer func() {
		_, _ = encoder.Drain()
		encoder.Close()
	}()
	opus, err := encoder.Encode(&audiocodec.PCMFrame{
		Samples:    make([]int16, 960*2),
		SampleRate: 48000,
		Channels:   2,
	})
	if err != nil {
		t.Fatalf("encode Opus fixture: %v", err)
	}
	for timestamp := int64(0); timestamp < 400; timestamp += 20 {
		if timestamp%40 == 0 {
			stream.WriteFrame(labmedia.VideoFrame(timestamp))
		}
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio,
			avframe.CodecOpus,
			avframe.FrameTypeInterframe,
			timestamp,
			timestamp,
			opus,
		))
	}

	session, err := h.module.StartLabSession(context.Background(), LabSessionRequest{
		Mode:      LabModeReceive,
		DeviceID:  "34020000001320000031",
		ChannelID: "34020000001320000032",
		StreamKey: stream.Key(),
	})
	if err != nil {
		t.Fatalf("StartLabSession receive: %v", err)
	}
	active := waitForGBLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive &&
			snapshot.RTPPacketsRecv > 0 && snapshot.RTCPPacketsRecv > 0 &&
			snapshot.PSFramesRecv > 0 && snapshot.AudioFramesRecv > 0 && snapshot.VideoFramesRecv > 0
	})
	if active.Direction != LabDirectionOutbound {
		t.Fatalf("receive direction = %q, want outbound", active.Direction)
	}
	if err := h.module.StopLabSession(session.ID); err != nil {
		t.Fatalf("StopLabSession receive: %v", err)
	}
}
