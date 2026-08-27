package gb28181

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
)

func TestGBOutboundSenderReportsArePeriodicAndCountPayloadOctets(t *testing.T) {
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, err := hub.GetOrCreate("gb28181/rtcp-sender")
	if err != nil {
		t.Fatalf("GetOrCreate stream: %v", err)
	}
	sender, err := newOutboundMediaSession(stream, 0, 0)
	if err != nil {
		t.Fatalf("newOutboundMediaSession: %v", err)
	}
	defer sender.close()
	remoteRTP, remoteRTCP, err := listenGBLabUDPPair()
	if err != nil {
		t.Fatalf("listen remote media pair: %v", err)
	}
	defer remoteRTP.Close()
	defer remoteRTCP.Close()
	if err := sender.setRemote(remoteRTP.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("setRemote: %v", err)
	}

	muxer := ps.NewMuxer()
	if err := sender.sendFrame(muxer, labmedia.VideoFrame(0)); err != nil {
		t.Fatalf("send first frame: %v", err)
	}
	firstTimestamp, firstPackets, firstOctets := readGBRTPFrame(t, remoteRTP)
	first := readGBSenderReport(t, remoteRTCP)

	time.Sleep(time.Second + 50*time.Millisecond)
	const secondDTS = int64(1200)
	if err := sender.sendFrame(muxer, labmedia.VideoFrame(secondDTS)); err != nil {
		t.Fatalf("send second frame: %v", err)
	}
	secondTimestamp, secondPackets, secondOctets := readGBRTPFrame(t, remoteRTP)
	second := readGBSenderReport(t, remoteRTCP)

	if first.SSRC != sender.ssrc || second.SSRC != sender.ssrc {
		t.Fatalf("sender-report SSRCs = %x/%x, want %x", first.SSRC, second.SSRC, sender.ssrc)
	}
	if first.RTPTime != firstTimestamp || second.RTPTime != secondTimestamp || second.RTPTime != uint32(secondDTS*90) {
		t.Fatalf("sender-report RTP timestamps = %d/%d, RTP frames = %d/%d", first.RTPTime, second.RTPTime, firstTimestamp, secondTimestamp)
	}
	if first.PacketCount != firstPackets || second.PacketCount != firstPackets+secondPackets {
		t.Fatalf("sender-report packet counts = %d/%d, want %d/%d", first.PacketCount, second.PacketCount, firstPackets, firstPackets+secondPackets)
	}
	if first.OctetCount != firstOctets || second.OctetCount != firstOctets+secondOctets {
		t.Fatalf("sender-report payload octets = %d/%d, want %d/%d", first.OctetCount, second.OctetCount, firstOctets, firstOctets+secondOctets)
	}
	if second.NTPTime <= first.NTPTime {
		t.Fatalf("sender-report NTP time did not advance: %d then %d", first.NTPTime, second.NTPTime)
	}
	if got := sender.rtcpPackets.Load(); got < 2 {
		t.Fatalf("sender reports sent = %d, want at least 2", got)
	}
}

func TestGBOutboundSessionStartupSkipsStaleHistory(t *testing.T) {
	hub := core.NewStreamHub(config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 32,
	}, config.LimitsConfig{}, core.NewEventBus())
	stream, err := hub.GetOrCreate("gb28181/stale-history")
	if err != nil {
		t.Fatal(err)
	}
	old := &gbOutboundTestPublisher{id: "publisher-a", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711A,
	}}
	if err := stream.SetPublisher(old); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x67, 0xaa},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x65, 0xaa},
	))

	stream.RemovePublisher()
	current := &gbOutboundTestPublisher{id: "publisher-b", info: old.info}
	if err := stream.SetPublisher(current); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x67, 0xbb},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0x65, 0xbb},
	))
	snapshot := stream.StartupSnapshot()

	sender, err := newOutboundMediaSession(stream, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.close()
	sender.snapshot = snapshot
	remoteRTP, remoteRTCP, err := listenGBLabUDPPair()
	if err != nil {
		t.Fatal(err)
	}
	defer remoteRTP.Close()
	defer remoteRTCP.Close()
	if err := sender.setRemote(remoteRTP.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	if err := sender.admit(); err != nil {
		t.Fatal(err)
	}
	sender.start()
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		33, 33, []byte{0x41, 0xbc},
	))

	var data []byte
	for i := 0; i < 3; i++ {
		if err := remoteRTP.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		packetData := make([]byte, 2048)
		n, _, err := remoteRTP.ReadFromUDP(packetData)
		if err != nil {
			t.Fatal(err)
		}
		var packet pionrtp.Packet
		if err := packet.Unmarshal(packetData[:n]); err != nil {
			t.Fatal(err)
		}
		data = append(data, packet.Payload...)
	}
	if bytes.Contains(data, []byte{0x65, 0xaa}) {
		t.Fatal("GB28181 outbound session sent publisher-A media from retained ring history")
	}
	if !bytes.Contains(data, []byte{0x65, 0xbb}) || !bytes.Contains(data, []byte{0x41, 0xbc}) {
		t.Fatalf("GB28181 outbound session missing publisher-B replay/live media: %x", data)
	}
}

type gbOutboundTestPublisher struct {
	id   string
	info *avframe.MediaInfo
}

func (p *gbOutboundTestPublisher) ID() string                    { return p.id }
func (p *gbOutboundTestPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *gbOutboundTestPublisher) Close() error                  { return nil }

func readGBSenderReport(t *testing.T, conn *net.UDPConn) *rtcp.SenderReport {
	t.Helper()
	buf := make([]byte, 2048)
	if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatalf("set RTCP read deadline: %v", err)
	}
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read RTCP sender report: %v", err)
	}
	packets, err := rtcp.Unmarshal(buf[:n])
	if err != nil {
		t.Fatalf("unmarshal RTCP sender report: %v", err)
	}
	for _, packet := range packets {
		if report, ok := packet.(*rtcp.SenderReport); ok {
			return report
		}
	}
	t.Fatalf("RTCP payload has no Sender Report: %#v", packets)
	return nil
}
