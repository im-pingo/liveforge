package gb28181

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	"github.com/im-pingo/liveforge/pkg/portalloc"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
)

func TestGBOutboundNegotiationCancelsWithPublisherGeneration(t *testing.T) {
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, err := hub.GetOrCreate("gb28181/negotiation-generation")
	if err != nil {
		t.Fatalf("GetOrCreate stream: %v", err)
	}
	if err := stream.SetPublisher(&gbOutboundTestPublisher{id: "publisher-a", info: &avframe.MediaInfo{
		VideoCodec:          avframe.CodecH264,
		VideoSequenceHeader: labmedia.VideoFrame(0).Payload,
		AudioCodec:          avframe.CodecG711A,
		SampleRate:          8000,
		Channels:            1,
	}}); err != nil {
		t.Fatalf("SetPublisher: %v", err)
	}
	ports, err := portalloc.New(42290, 42291)
	if err != nil {
		t.Fatalf("New port allocator: %v", err)
	}
	h := &handler{sessions: NewSessionManager(), hub: hub, ports: ports}
	releaseInvite := make(chan struct{})
	inviteContext := make(chan context.Context, 1)
	var dialog *successfulInviteDialog
	var releaseOnce sync.Once
	m := &Module{
		sipService: failingInviteService{},
		handler:    h,
		sendInvite: func(ctx context.Context, req *sip.Request) (inviteDialog, error) {
			inviteContext <- ctx
			<-releaseInvite
			dialog = newSuccessfulInviteDialog(req)
			return dialog, nil
		},
	}
	defer releaseOnce.Do(func() { close(releaseInvite) })

	result := make(chan error, 1)
	go func() {
		_, err := m.startOutboundMedia(context.Background(), &Device{
			DeviceID: "device", RemoteAddr: "127.0.0.1:5060", Transport: "udp",
		}, "channel", stream.Key())
		result <- err
	}()

	var bound context.Context
	select {
	case bound = <-inviteContext:
	case <-time.After(time.Second):
		t.Fatal("GB28181 outbound INVITE did not start")
	}
	stream.RemovePublisher()
	select {
	case <-bound.Done():
	case <-time.After(time.Second):
		t.Fatal("GB28181 outbound INVITE context was not canceled with publisher generation")
	}
	releaseOnce.Do(func() { close(releaseInvite) })

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("GB28181 outbound negotiation activated a retired publisher generation")
		}
		if errors.Is(err, context.Canceled) {
			t.Fatalf("GB28181 generation retirement returned caller cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("GB28181 outbound negotiation did not terminate after publisher retirement")
	}
	if dialog.ackCalls.Load() != 0 {
		t.Fatalf("GB28181 stale negotiation sent %d ACKs, want none", dialog.ackCalls.Load())
	}
	if got := len(h.sessions.All()); got != 0 {
		t.Fatalf("GB28181 stale negotiation activated %d sessions", got)
	}
}

func TestGBOutboundWaitsForPublisherReadinessBeforeSendingInvite(t *testing.T) {
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, err := hub.GetOrCreate("gb28181/late-header")
	if err != nil {
		t.Fatalf("GetOrCreate stream: %v", err)
	}
	if err := stream.SetPublisher(&gbOutboundTestPublisher{id: "late-header", info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: avframe.CodecG711A,
		SampleRate: 8000,
		Channels:   1,
	}}); err != nil {
		t.Fatalf("SetPublisher: %v", err)
	}
	ports, err := portalloc.New(42320, 42321)
	if err != nil {
		t.Fatalf("New port allocator: %v", err)
	}
	h := &handler{sessions: NewSessionManager(), hub: hub, ports: ports}
	inviteEntered := make(chan struct{})
	m := &Module{
		sipService: failingInviteService{},
		handler:    h,
		sendInvite: func(ctx context.Context, _ *sip.Request) (inviteDialog, error) {
			close(inviteEntered)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := m.startOutboundMedia(ctx, &Device{
			DeviceID: "device", RemoteAddr: "127.0.0.1:5060", Transport: "udp",
		}, "channel", stream.Key())
		result <- err
	}()

	select {
	case <-inviteEntered:
		t.Fatal("GB28181 outbound sent INVITE before the video sequence header was ready")
	case <-time.After(50 * time.Millisecond):
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x42, 0x00, 0x1e, 0xff},
	))
	select {
	case <-inviteEntered:
	case <-time.After(time.Second):
		t.Fatal("GB28181 outbound did not send INVITE after the publisher became ready")
	}
	cancel()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("GB28181 outbound did not stop after test cancellation")
	}
}

func TestGBOutboundGenerationRetirementAfterAccepted2xxSendsBYE(t *testing.T) {
	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, err := hub.GetOrCreate("gb28181/accepted-generation-retirement")
	if err != nil {
		t.Fatalf("GetOrCreate stream: %v", err)
	}
	if err := stream.SetPublisher(&gbOutboundTestPublisher{id: "publisher-a", info: &avframe.MediaInfo{
		VideoCodec:          avframe.CodecH264,
		VideoSequenceHeader: labmedia.VideoFrame(0).Payload,
		AudioCodec:          avframe.CodecG711A,
		SampleRate:          8000,
		Channels:            1,
	}}); err != nil {
		t.Fatalf("SetPublisher: %v", err)
	}
	ports, err := portalloc.New(42300, 42301)
	if err != nil {
		t.Fatalf("New port allocator: %v", err)
	}
	h := &handler{sessions: NewSessionManager(), hub: hub, ports: ports}
	inviteContext := make(chan context.Context, 1)
	var dialog *successfulInviteDialog
	m := &Module{
		sipService: failingInviteService{},
		handler:    h,
		sendInvite: func(ctx context.Context, req *sip.Request) (inviteDialog, error) {
			inviteContext <- ctx
			done := make(chan struct{})
			response := sip.NewResponseFromRequest(req, 200, "OK", []byte("v=0\r\nm=video 30000 RTP/AVP 96\r\n"))
			response.AppendHeader(sip.NewHeader("To", "<sip:channel@127.0.0.1>;tag=accepted"))
			dialog = &successfulInviteDialog{response: response, done: done}
			stream.RemovePublisher()
			return dialog, nil
		},
	}

	result := make(chan error, 1)
	go func() {
		_, err := m.startOutboundMedia(context.Background(), &Device{
			DeviceID: "device", RemoteAddr: "127.0.0.1:5060", Transport: "udp",
		}, "channel", stream.Key())
		result <- err
	}()

	select {
	case bound := <-inviteContext:
		select {
		case <-bound.Done():
		case <-time.After(time.Second):
			t.Fatal("GB28181 outbound INVITE context was not canceled with publisher generation")
		}
	case <-time.After(time.Second):
		t.Fatal("GB28181 outbound INVITE did not start")
	}

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "GB28181 outbound media source generation ended") {
			t.Fatalf("GB28181 outbound error = %v, want source-generation retirement", err)
		}
	case <-time.After(time.Second):
		t.Fatal("GB28181 outbound negotiation did not terminate after publisher retirement")
	}
	if dialog.ackCalls.Load() != 0 {
		t.Fatalf("GB28181 stale negotiation sent %d ACKs, want none", dialog.ackCalls.Load())
	}
	if dialog.byeCalls.Load() != 1 || !dialog.closed.Load() {
		t.Fatalf("accepted GB28181 dialog cleanup BYE=%d closed=%v, want 1/true", dialog.byeCalls.Load(), dialog.closed.Load())
	}
}

func TestGBOutboundSkipsExternallyOccupiedFirstPortPair(t *testing.T) {
	portRange := freeGBLabRTPPortRange(t, 2)
	loopback := net.ParseIP("127.0.0.1")
	occupiedRTP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback, Port: portRange[0]})
	if err != nil {
		t.Fatalf("occupy first RTP port: %v", err)
	}
	defer occupiedRTP.Close()
	occupiedRTCP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback, Port: portRange[0] + 1})
	if err != nil {
		t.Fatalf("occupy first RTCP port: %v", err)
	}
	defer occupiedRTCP.Close()

	hub := core.NewStreamHub(config.StreamConfig{RingBufferSize: 16}, config.LimitsConfig{}, core.NewEventBus())
	stream, err := hub.GetOrCreate("gb28181/occupied-first-pair")
	if err != nil {
		t.Fatalf("GetOrCreate stream: %v", err)
	}
	if err := stream.SetPublisher(&gbOutboundTestPublisher{id: "publisher-a", info: &avframe.MediaInfo{
		VideoCodec:          avframe.CodecH264,
		VideoSequenceHeader: labmedia.VideoFrame(0).Payload,
		AudioCodec:          avframe.CodecG711A,
		SampleRate:          8000,
		Channels:            1,
	}}); err != nil {
		t.Fatalf("SetPublisher: %v", err)
	}
	ports, err := portalloc.New(portRange[0], portRange[1])
	if err != nil {
		t.Fatalf("New port allocator: %v", err)
	}
	sessions := NewSessionManager()
	h := &handler{sessions: sessions, hub: hub, bus: core.NewEventBus(), ports: ports}
	remoteRTP, remoteRTCP, err := listenGBLabUDPPair()
	if err != nil {
		t.Fatalf("listen remote media pair: %v", err)
	}
	defer remoteRTP.Close()
	defer remoteRTCP.Close()
	m := &Module{
		sipService: failingInviteService{},
		handler:    h,
		sessions:   sessions,
		sendInvite: func(_ context.Context, req *sip.Request) (inviteDialog, error) {
			dialog := newSuccessfulInviteDialog(req)
			dialog.response.SetBody(buildGBLabSDP(remoteRTP.LocalAddr().(*net.UDPAddr).Port, "recvonly"))
			return dialog, nil
		},
	}

	session, err := m.startOutboundMedia(context.Background(), &Device{
		DeviceID: "device", RemoteAddr: "127.0.0.1:5060", Transport: "udp",
	}, "channel", stream.Key())
	if err != nil {
		t.Fatalf("startOutboundMedia with occupied first pair: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			h.closeSession(session, "")
		}
	})
	if session.LocalPort != portRange[0]+2 {
		t.Fatalf("outbound local RTP port = %d, want second pair %d", session.LocalPort, portRange[0]+2)
	}
	sender := session.Snapshot().Sender
	if sender == nil {
		t.Fatal("outbound session has no media sender")
	}
	if got := sender.rtpConn.LocalAddr().(*net.UDPAddr).Port; got != portRange[0]+2 {
		t.Fatalf("sender RTP socket = %d, want %d", got, portRange[0]+2)
	}
	if got := sender.rtcpConn.LocalAddr().(*net.UDPAddr).Port; got != portRange[0]+3 {
		t.Fatalf("sender RTCP socket = %d, want %d", got, portRange[0]+3)
	}
	if !h.closeSession(session, "") {
		t.Fatal("outbound session did not own cleanup")
	}
	closed = true
	reused, err := ports.AllocateBoundUDPPair("udp4", loopback)
	if err != nil {
		t.Fatalf("outbound cleanup did not release second pair: %v", err)
	}
	if reused.RTPPort != portRange[0]+2 || reused.RTCPPort != portRange[0]+3 {
		t.Fatalf("reused pair = %d/%d, want %d/%d", reused.RTPPort, reused.RTCPPort, portRange[0]+2, portRange[0]+3)
	}
	closeBoundUDPPair(reused)
	ports.Free(reused.RTPPort, reused.RTCPPort)
}

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
