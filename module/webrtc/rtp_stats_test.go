package webrtc

import (
	"testing"

	"github.com/pion/rtp"
)

func TestRTPPacketWireSizeCountsHeaderAndPayload(t *testing.T) {
	header := &rtp.Header{Version: 2}

	got, ok := rtpPacketWireSize(header, []byte{1, 2, 3})
	if !ok || got != 15 {
		t.Fatalf("rtpPacketWireSize = (%d, %v), want (15, true)", got, ok)
	}
}

func TestCheckedUint64RejectsNegativeValue(t *testing.T) {
	if got, ok := checkedUint64(-1); ok || got != 0 {
		t.Fatalf("checkedUint64(-1) = (%d, %v), want (0, false)", got, ok)
	}
}
