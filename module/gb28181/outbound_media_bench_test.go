package gb28181

import (
	"io"
	"net"
	"testing"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
)

func BenchmarkGBOutboundSendFrame(b *testing.B) {
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, err := hub.GetOrCreate("gb28181/benchmark")
	if err != nil {
		b.Fatal(err)
	}
	sender, err := newOutboundMediaSession(stream, 0, 0)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(sender.close)
	remoteRTP, remoteRTCP, err := listenGBLabUDPPair()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = remoteRTP.Close()
		_ = remoteRTCP.Close()
	})
	if err := sender.setRemote(remoteRTP.LocalAddr().(*net.UDPAddr)); err != nil {
		b.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, remoteRTP) }()
	go func() { _, _ = io.Copy(io.Discard, remoteRTCP) }()
	muxer := ps.NewMuxer()
	frame := labmedia.VideoFrame(0)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sender.sendFrame(muxer, frame); err != nil {
			b.Fatal(err)
		}
	}
}
