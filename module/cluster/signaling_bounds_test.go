package cluster

import (
	"fmt"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

func TestRelayMediaSessionRejectsInvalidNumericNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name, clockRate string
		payloadType     int
		valid           bool
	}{
		{"h264", "90000", 96, true},
		{"negative payload type", "90000", -1, false},
		{"payload type exceeds seven bits", "90000", 128, false},
		{"payload type wraps byte", "90000", 256, false},
		{"zero clock rate", "0", 96, false},
		{"negative clock rate", "-1", 96, false},
		{"clock rate exceeds uint32", "4294967296", 96, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := &sdp.SessionDescription{Media: []*sdp.MediaDescription{{
				Type: "video", Formats: []int{tc.payloadType},
				Attributes: []sdp.Attribute{{Key: "rtpmap", Value: fmt.Sprintf("%d H264/%s", tc.payloadType, tc.clockRate)}},
			}}}
			session := relayMediaSession(document, "video")
			if (session != nil) != tc.valid {
				t.Fatalf("session=%+v, valid=%v", session, tc.valid)
			}
			if tc.valid && (session.PayloadType != 96 || session.ClockRate != 90000) {
				t.Errorf("valid RTP mapping changed: %+v", session)
			}
		})
	}
}

func TestGBSendPullRejectsInvalidPortBeforeSetup(t *testing.T) {
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, err := hub.GetOrCreate("live/invalid-port")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Remove(stream.Key()) })
	for _, port := range []int{-1, 0, 65536} {
		transport := &GBTransport{}
		if sendErr := transport.sendPull(stream, stream.StartupSnapshot(), "127.0.0.1:19000", port); sendErr == nil {
			t.Errorf("port%d was accepted", port)
		}
	}
}
