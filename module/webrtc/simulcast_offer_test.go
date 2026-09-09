package webrtc

import (
	"strings"
	"testing"

	"github.com/im-pingo/liveforge/config"
)

func simulcastTestConfig(pause bool) config.SimulcastConfig {
	return config.SimulcastConfig{Enabled: true, AutoPauseLayer: pause, Layers: []config.LayerConfig{
		{RID: "m", MaxBitrate: 1000000}, {RID: "l", MaxBitrate: 500000}, {RID: "h", MaxBitrate: 2500000},
	}}
}

func simulcastTestOffer(attrs string) string {
	return "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\nc=IN IP4 0.0.0.0\r\na=rtpmap:96 VP8/90000\r\na=sendonly\r\n" + attrs
}

func TestWHIPSimulcastOfferAdmission(t *testing.T) {
	attrs := "a=rid:l send\r\na=rid:h send\r\na=rid:m send\r\na=simulcast:send l;h;m\r\n"
	plan, err := parseWHIPSimulcastOffer(simulcastTestOffer(attrs), simulcastTestConfig(true))
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || len(plan.layers) != 3 || plan.layers[0].RID != "h" || plan.layers[2].RID != "l" {
		t.Fatalf("unexpected ranking: %+v", plan)
	}
	for _, tc := range []struct {
		name, attrs string
		cfg         config.SimulcastConfig
	}{
		{"disabled", attrs, config.SimulcastConfig{}},
		{"duplicate", attrs + "a=rid:h send\r\n", simulcastTestConfig(false)},
		{"unknown", strings.ReplaceAll(attrs, "m", "x"), simulcastTestConfig(false)},
		{"undeclared", strings.ReplaceAll(attrs, "l;h;m", "l;h;x"), simulcastTestConfig(false)},
		{"alternatives", strings.ReplaceAll(attrs, "l;h;m", "l,h;m"), simulcastTestConfig(false)},
		{"paused", strings.ReplaceAll(attrs, "l;h;m", "l;~h;m"), simulcastTestConfig(false)},
		{"legacy", "a=ssrc-group:SIM 1 2 3\r\n", simulcastTestConfig(false)},
		{"missing simulcast", "a=rid:h send\r\n", simulcastTestConfig(false)},
		{"empty simulcast", "a=simulcast:\r\n", simulcastTestConfig(false)},
		{"too many", "a=rid:h send\r\na=rid:l send\r\na=rid:m send\r\na=rid:x send\r\na=simulcast:send h;l;m;x\r\n", simulcastTestConfig(false)},
		{"receiving", strings.ReplaceAll(attrs, "send", "recv"), simulcastTestConfig(false)},
		{"second video", attrs + "m=video 9 UDP/TLS/RTP/SAVPF 96\r\na=sendonly\r\na=rtpmap:96 VP8/90000\r\n", simulcastTestConfig(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseWHIPSimulcastOffer(simulcastTestOffer(tc.attrs), tc.cfg); err == nil {
				t.Fatal("accepted unsupported simulcast offer")
			}
		})
	}
	if plan, err := parseWHIPSimulcastOffer(simulcastTestOffer(""), config.SimulcastConfig{}); err != nil || plan != nil {
		t.Fatalf("single layer changed: plan=%+v err=%v", plan, err)
	}
}
