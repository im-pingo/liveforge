package rtmp

import (
	"reflect"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestPeerCapabilitiesSupportAndWildcard(t *testing.T) {
	caps := ParsePeerCapabilities(map[string]any{
		"fourCcList": []any{"vp09", "Opus"},
		"videoFourCcInfoMap": map[string]any{
			"vp09": float64(CanDecode),
			"av01": float64(CanEncode),
		},
		"audioFourCcInfoMap": map[string]any{
			"Opus": float64(CanForward),
		},
		"capsEx": float64(0),
	})
	if !caps.SupportsVideo(avframe.CodecVP9) {
		t.Error("VP9 should be supported by CanDecode")
	}
	if caps.SupportsVideo(avframe.CodecAV1) {
		t.Error("CanEncode alone should not permit playback")
	}
	if !caps.SupportsAudio(avframe.CodecOpus) {
		t.Error("Opus should be supported by CanForward")
	}

	wildcard := ParsePeerCapabilities(map[string]any{
		"videoFourCcInfoMap": map[string]any{
			"*":    float64(CanEncode),
			"vp09": float64(CanDecode),
		},
	})
	if wildcard.SupportsVideo(avframe.CodecVP9) {
		t.Error("wildcard should override the specific VP9 flag")
	}
}

func TestPeerCapabilitiesLegacyAndMalformedInput(t *testing.T) {
	legacy := ParsePeerCapabilities(nil)
	if legacy.Enhanced() {
		t.Error("empty capabilities should represent a legacy peer")
	}
	if legacy.SupportsVideo(avframe.CodecH265) {
		t.Error("legacy peer should not advertise enhanced H.265")
	}

	malformed := ParsePeerCapabilities(map[string]any{
		"fourCcList":         "vp09",
		"videoFourCcInfoMap": 42,
		"audioFourCcInfoMap": map[string]any{"Opus": "bad"},
		"capsEx":             "bad",
	})
	if malformed.Enhanced() {
		t.Error("malformed optional fields should degrade to legacy")
	}
}

func TestServerCapabilitiesObject(t *testing.T) {
	obj := ServerCapabilitiesObject()
	wantList := []any{"vp08", "vp09", "av01", "avc1", "hvc1", "mp4a", "Opus", ".mp3"}
	if got, ok := obj["fourCcList"].([]any); !ok || !reflect.DeepEqual(got, wantList) {
		t.Fatalf("fourCcList = %#v, want %#v", obj["fourCcList"], wantList)
	}
	if got, ok := obj["capsEx"].(float64); !ok || got != 0 {
		t.Fatalf("capsEx = %#v, want 0", obj["capsEx"])
	}
	videoMap, ok := obj["videoFourCcInfoMap"].(map[string]any)
	if !ok || videoMap["vp09"] != float64(CanForward) {
		t.Fatalf("video map = %#v", obj["videoFourCcInfoMap"])
	}
	audioMap, ok := obj["audioFourCcInfoMap"].(map[string]any)
	if !ok || audioMap["Opus"] != float64(CanForward) {
		t.Fatalf("audio map = %#v", obj["audioFourCcInfoMap"])
	}
}
