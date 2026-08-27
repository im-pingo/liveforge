package labmedia

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/h264"
)

func TestVideoFrameLoopsAtOneSecondIDRInterval(t *testing.T) {
	first := VideoFrame(0)
	next := VideoFrame(VideoFrameDurationMs)
	looped := VideoFrame(25 * VideoFrameDurationMs)

	if first.FrameType != avframe.FrameTypeKeyframe || looped.FrameType != avframe.FrameTypeKeyframe {
		t.Fatalf("frame types at 0/1000ms = %v/%v, want keyframes", first.FrameType, looped.FrameType)
	}
	if next.FrameType != avframe.FrameTypeInterframe {
		t.Fatalf("frame type at 40ms = %v, want interframe", next.FrameType)
	}
	if bytes.Equal(first.Payload, next.Payload) {
		t.Fatal("consecutive video frames are identical")
	}
}

func TestVideoFrameCarriesDecodableH264Parameters(t *testing.T) {
	var sps []byte
	for _, nalu := range h264.ExtractNALUs(VideoFrame(0).Payload) {
		if len(nalu) > 0 && nalu[0]&0x1f == h264.NALTypeSPS {
			sps = nalu
			break
		}
	}
	if len(sps) == 0 {
		t.Fatal("keyframe has no H.264 SPS")
	}

	info, err := h264.ParseSPS(sps)
	if err != nil {
		t.Fatalf("ParseSPS: %v", err)
	}
	if info.Width < 160 || info.Height < 90 {
		t.Fatalf("SPS dimensions = %dx%d, want at least 160x90", info.Width, info.Height)
	}
}

func TestG711FrameIsAudibleTwentyMillisecondAudio(t *testing.T) {
	for _, codec := range []avframe.CodecType{avframe.CodecG711A, avframe.CodecG711U} {
		frame := G711Frame(codec, 0)
		if frame.Codec != codec || frame.MediaType != avframe.MediaTypeAudio || frame.DTS != 0 || frame.PTS != 0 {
			t.Errorf("%s frame metadata = %+v", codec, frame)
		}
		if len(frame.Payload) != 160 {
			t.Errorf("%s payload length = %d, want 160 samples", codec, len(frame.Payload))
			continue
		}
		if bytes.Equal(frame.Payload[:20], frame.Payload[20:40]) {
			t.Errorf("%s positive and negative square-wave halves are identical", codec)
		}
	}
}
