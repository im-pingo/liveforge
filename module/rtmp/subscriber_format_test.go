package rtmp

import (
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	flvpkg "github.com/im-pingo/liveforge/pkg/muxer/flv"
)

func TestChooseOutputPolicyLegacy(t *testing.T) {
	policy, err := chooseOutputPolicy(&avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecAAC,
	}, PeerCapabilities{})
	if err != nil {
		t.Fatalf("chooseOutputPolicy: %v", err)
	}
	if policy.videoMode != flvpkg.EncodingClassic || policy.audioMode != flvpkg.EncodingClassic {
		t.Fatalf("policy modes = %d/%d, want classic/classic", policy.videoMode, policy.audioMode)
	}
	if policy.transcodeAudio {
		t.Error("AAC should not require transcoding")
	}
}

func TestChooseOutputPolicyEnhancedPerTrack(t *testing.T) {
	caps := ParsePeerCapabilities(map[string]any{
		"videoFourCcInfoMap": map[string]any{"avc1": float64(CanDecode)},
		"audioFourCcInfoMap": map[string]any{"Opus": float64(CanForward)},
	})
	policy, err := chooseOutputPolicy(&avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecOpus,
	}, caps)
	if err != nil {
		t.Fatalf("chooseOutputPolicy: %v", err)
	}
	if policy.videoMode != flvpkg.EncodingEnhanced || policy.audioMode != flvpkg.EncodingEnhanced {
		t.Fatalf("policy modes = %d/%d, want enhanced/enhanced", policy.videoMode, policy.audioMode)
	}
	if policy.transcodeAudio {
		t.Error("advertised Opus should not require transcoding")
	}
}

func TestChooseOutputPolicyFallbacks(t *testing.T) {
	policy, err := chooseOutputPolicy(&avframe.MediaInfo{AudioCodec: avframe.CodecOpus}, PeerCapabilities{})
	if err != nil {
		t.Fatalf("chooseOutputPolicy Opus: %v", err)
	}
	if policy.audioMode != flvpkg.EncodingClassic || !policy.transcodeAudio {
		t.Fatalf("Opus fallback = mode %d/transcode %v", policy.audioMode, policy.transcodeAudio)
	}

	if _, err := chooseOutputPolicy(&avframe.MediaInfo{VideoCodec: avframe.CodecH265}, PeerCapabilities{}); err == nil {
		t.Fatal("legacy peer should reject H.265 output")
	}
}

func TestSubscriberEnhancedPayloadHasNoExtraAudioPacketByte(t *testing.T) {
	sub := &Subscriber{muxer: flvpkg.NewMuxerWithModes(flvpkg.EncodingEnhanced, flvpkg.EncodingEnhanced)}
	payload, err := sub.buildRTMPPayload(avframe.NewAVFrame(
		avframe.MediaTypeAudio,
		avframe.CodecOpus,
		avframe.FrameTypeInterframe,
		0,
		0,
		[]byte{0xde, 0xad},
	))
	if err != nil {
		t.Fatalf("buildRTMPPayload: %v", err)
	}
	wantPrefix := []byte{0x91, 'O', 'p', 'u', 's', 0xde, 0xad}
	if len(payload) != len(wantPrefix) {
		t.Fatalf("payload length = %d, want %d", len(payload), len(wantPrefix))
	}
	for i, want := range wantPrefix {
		if payload[i] != want {
			t.Errorf("payload[%d] = 0x%02x, want 0x%02x", i, payload[i], want)
		}
	}
}
