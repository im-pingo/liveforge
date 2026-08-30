package sipgateway

import (
	"io"
	"math"
	"net"
	"testing"

	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/avframe"
	mediarp "github.com/im-pingo/liveforge/pkg/rtp"
)

func BenchmarkSIPOutboundSendFrame(b *testing.B) {
	remoteRTP, remoteRTCP, err := listenLabUDPPair()
	if err != nil {
		b.Fatal(err)
	}
	sendRTP, sendRTCP, err := listenLabUDPPair()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = remoteRTP.Close()
		_ = remoteRTCP.Close()
		_ = sendRTP.Close()
		_ = sendRTCP.Close()
	})
	go func() { _, _ = io.Copy(io.Discard, remoteRTP) }()
	go func() { _, _ = io.Copy(io.Discard, remoteRTCP) }()
	codec := negotiatedCodec{Codec: avframe.CodecG711A, PT: 8, ClockRate: 8000, EncodingName: "PCMA"}
	if codec.PT < 0 || codec.PT > math.MaxUint8 || codec.ClockRate <= 0 {
		b.Fatal("benchmark codec has invalid RTP parameters")
	}
	call := newCallSession("benchmark", "sip/benchmark", codec, "outbound", 0, 0)
	packetizer, err := mediarp.NewPacketizer(codec.Codec)
	if err != nil {
		b.Fatal(err)
	}
	session := mediarp.NewSession(uint8(codec.PT), uint32(codec.ClockRate)) //nolint:gosec // parameters validated above
	frame := labmedia.G711Frame(codec.Codec, 0)
	var reportState rtcpSenderState
	remoteAddr := remoteRTP.LocalAddr().(*net.UDPAddr)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !call.sendFrame(frame, packetizer, session, sendRTP, sendRTCP, remoteAddr, &reportState) {
			b.Fatal("sendFrame failed")
		}
	}
}
