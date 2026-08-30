//go:build audiocodec

package sipgateway

import (
	"context"
	"testing"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestSIPLabReceiveTranscodesPCMAToRequestedPCMU(t *testing.T) {
	h := newRealSIPLabHarness(t)
	stream, err := h.hub.GetOrCreate("sip/receive-pcma-to-pcmu")
	if err != nil {
		t.Fatalf("GetOrCreate receive stream: %v", err)
	}
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 256))
	if setErr := stream.SetPublisher(&gatewayTestPublisher{
		id: "sip-lab-pcma-source",
		info: &avframe.MediaInfo{
			VideoCodec:          avframe.CodecH264,
			VideoSequenceHeader: labmedia.VideoFrame(0).Payload,
			AudioCodec:          avframe.CodecG711A,
			SampleRate:          8000,
			Channels:            1,
		},
	}); setErr != nil {
		t.Fatalf("SetPublisher receive source: %v", setErr)
	}

	session, err := h.module.StartLabSession(context.Background(), LabSessionRequest{
		Mode:      LabModeReceive,
		DeviceID:  "pcmu-receive-device",
		StreamKey: stream.Key(),
		Codec:     "PCMU",
	})
	if err != nil {
		t.Fatalf("StartLabSession receive: %v", err)
	}
	for timestamp := int64(0); timestamp < 400; timestamp += 20 {
		if timestamp%40 == 0 {
			stream.WriteFrame(labmedia.VideoFrame(timestamp))
		}
		stream.WriteFrame(labmedia.G711Frame(avframe.CodecG711A, timestamp))
	}

	active := waitForSIPLabSnapshot(t, h.module, session.ID, func(snapshot LabSessionSnapshot) bool {
		return snapshot.State == LabSessionStateActive &&
			snapshot.AudioRTPPacketsRecv > 0 && snapshot.VideoRTPPacketsRecv > 0
	})
	if active.Codec != "PCMU" {
		t.Fatalf("receive codec = %q, want PCMU", active.Codec)
	}
	if err := h.module.StopLabSession(session.ID); err != nil {
		t.Fatalf("StopLabSession receive: %v", err)
	}
}
