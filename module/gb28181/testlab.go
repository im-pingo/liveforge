package gb28181

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	"github.com/im-pingo/liveforge/pkg/protocoltest"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
)

// RunSelfTest exercises GB28181 media primitives locally. It does not
// register a device, send SIP messages, or require an external platform.
func (m *Module) RunSelfTest(ctx context.Context) (protocoltest.Report, error) {
	started := time.Now()
	checks := make([]protocoltest.Check, 0, 16)
	if err := ctx.Err(); err != nil {
		return protocoltest.NewWithDuration("gb28181", []protocoltest.Check{{Name: "context", Detail: err.Error()}}, time.Since(started)), nil
	}
	if m.handler == nil || m.handler.ports == nil {
		return protocoltest.NewWithDuration("gb28181", []protocoltest.Check{{Name: "module_initialized", Detail: "GB28181 module is not initialized"}}, time.Since(started)), nil
	}
	rtpPort, rtcpPort, err := m.handler.ports.AllocatePair()
	if err != nil {
		checks = append(checks, protocoltest.Check{Name: "rtp_port_allocation", Detail: err.Error()})
	} else {
		m.handler.ports.Free(rtpPort, rtcpPort)
		checks = append(checks, protocoltest.Check{Name: "rtp_port_allocation", Passed: true, Detail: fmt.Sprintf("%d/%d", rtpPort, rtcpPort)})
	}

	muxer := ps.NewMuxer()
	payload := []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0x1e, 0xab, 0x40, 0x50, 0, 0, 0, 1, 0x68, 0xce, 0x38, 0x80, 0, 0, 0, 1, 0x65, 0x01, 0x02, 0x03}
	packed, err := muxer.Pack(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, payload))
	if err != nil {
		checks = append(checks, protocoltest.Check{Name: "ps_roundtrip", Detail: err.Error()})
	} else {
		demuxer := ps.NewDemuxer()
		frames, demuxErr := demuxer.Feed(packed)
		passed := demuxErr == nil && len(frames) > 0 && demuxer.VideoCodec() == avframe.CodecH264
		detail := fmt.Sprintf("frames=%d codec=%s", len(frames), demuxer.VideoCodec())
		if demuxErr != nil {
			detail = demuxErr.Error()
		}
		checks = append(checks, protocoltest.Check{Name: "ps_roundtrip", Passed: passed, Detail: detail})
	}
	checks = append(checks, runGBSignalingLoop(ctx)...)
	checks = append(checks, runGBMediaLoop(ctx)...)
	return protocoltest.NewWithDuration("gb28181", checks, time.Since(started)), nil
}

type gbLabDevice struct {
	registered bool
	invited    bool
	acked      bool
	closed     bool
}

func runGBSignalingLoop(ctx context.Context) []protocoltest.Check {
	checks := make([]protocoltest.Check, 0, 8)
	if err := ctx.Err(); err != nil {
		return []protocoltest.Check{{Name: "signaling_context", Detail: err.Error()}}
	}
	const deviceID = "34020000001320000001"
	registry := NewDeviceRegistry(time.Minute, "")
	defer registry.Stop()
	device := &gbLabDevice{}
	registered := device.register()
	if registered {
		registry.Register(deviceID, "127.0.0.1:5060", "udp")
	}
	checks = append(checks, protocoltest.Check{Name: "register", Passed: registered && registry.Get(deviceID) != nil, Detail: "fake device registered through local SIP peer"})
	keepalive, err := ParseKeepalive([]byte("<?xml version=\"1.0\"?><Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>" + deviceID + "</DeviceID><Status>OK</Status></Notify>"))
	if err == nil && keepalive.DeviceID == deviceID && keepalive.Status == "OK" {
		registry.Keepalive(deviceID)
	}
	checks = append(checks, protocoltest.Check{Name: "keepalive", Passed: err == nil && keepalive.DeviceID == deviceID && registry.Get(deviceID).Status == DeviceStatusOnline, Detail: "keepalive refreshed fake device state"})

	query := BuildCatalogQuery(2, deviceID)
	catalogXML := []byte("<?xml version=\"1.0\"?><Response><CmdType>Catalog</CmdType><SN>2</SN><DeviceID>" + deviceID + "</DeviceID><SumNum>1</SumNum><DeviceList Num=\"1\"><Item><DeviceID>34020000001320000002</DeviceID><Name>Lab Camera</Name><Manufacturer>LiveForge</Manufacturer><Status>ON</Status><PTZType>0</PTZType></Item></DeviceList></Response>")
	catalog, catalogErr := ParseCatalogResponse(catalogXML)
	checks = append(checks, protocoltest.Check{Name: "catalog", Passed: ParseMessageType(query) == "Catalog" && catalogErr == nil && catalog.SumNum == 1 && len(catalog.DeviceList.Items) == 1, Detail: "catalog query/response completed in local fake peer"})

	invite := sip.NewRequest(sip.INVITE, sip.Uri{User: "34020000001320000002", Host: "lab.local"})
	invite.SetBody([]byte("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=GB lab\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=video 41000 RTP/AVP 96\r\na=recvonly\r\na=rtpmap:96 PS/90000\r\n"))
	status, answer := device.invite(invite)
	checks = append(checks, protocoltest.Check{Name: "play_invite_200_ok", Passed: status == 200 && device.invited, Detail: fmt.Sprintf("status=%d", status)})
	checks = append(checks, protocoltest.Check{Name: "play_sdp_answer", Passed: strings.Contains(answer, "m=video") && strings.Contains(answer, "a=rtpmap:96 PS/90000"), Detail: "fake device returned PS/90000 SDP answer"})
	checks = append(checks, protocoltest.Check{Name: "ack", Passed: device.ack(), Detail: "ACK acknowledged by fake device"})
	checks = append(checks, protocoltest.Check{Name: "bye", Passed: device.bye(), Detail: "BYE closed fake device dialog"})

	badInvite := sip.NewRequest(sip.INVITE, sip.Uri{User: "34020000001320000002", Host: "lab.local"})
	badStatus, _ := device.invite(badInvite)
	checks = append(checks, protocoltest.Check{Name: "invite_rejection", Passed: badStatus == 400, Detail: fmt.Sprintf("status=%d missing SDP", badStatus)})
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	err = waitForGBLabResponse(timeoutCtx)
	checks = append(checks, protocoltest.Check{Name: "invite_timeout", Passed: errors.Is(err, context.DeadlineExceeded), Detail: gbErrString(err)})
	return checks
}

func (d *gbLabDevice) register() bool {
	d.registered = true
	return d.registered
}

func (d *gbLabDevice) invite(req *sip.Request) (int, string) {
	if !d.registered {
		return 403, ""
	}
	if len(req.Body()) == 0 {
		return 400, ""
	}
	d.invited = true
	return 200, "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=GB lab answer\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=video 41002 RTP/AVP 96\r\na=sendonly\r\na=rtpmap:96 PS/90000\r\n"
}

func (d *gbLabDevice) ack() bool {
	d.acked = d.invited
	return d.acked
}

func (d *gbLabDevice) bye() bool {
	d.closed = d.acked
	return d.closed
}

func waitForGBLabResponse(ctx context.Context) error {
	select {
	case <-time.After(100 * time.Millisecond):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func gbErrString(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func runGBMediaLoop(ctx context.Context) []protocoltest.Check {
	mediaListener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return []protocoltest.Check{{Name: "ps_rtp_media_loopback", Detail: err.Error()}, {Name: "rtcp_control_loopback", Detail: err.Error()}}
	}
	defer mediaListener.Close()
	rtcpListener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return []protocoltest.Check{{Name: "ps_rtp_media_loopback", Detail: err.Error()}, {Name: "rtcp_control_loopback", Detail: err.Error()}}
	}
	defer rtcpListener.Close()
	muxer := ps.NewMuxer()
	packed, err := muxer.Pack(avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0, 0, 0, 1, 0x67, 0x42, 0, 0x1e, 0, 0, 0, 1, 0x65, 1, 2, 3}))
	if err != nil {
		return []protocoltest.Check{{Name: "ps_rtp_media_loopback", Detail: err.Error()}, {Name: "rtcp_control_loopback", Detail: err.Error()}}
	}
	rtcpData, err := (&rtcp.ReceiverReport{SSRC: 7}).Marshal()
	if err != nil {
		return []protocoltest.Check{{Name: "ps_rtp_media_loopback", Detail: err.Error()}, {Name: "rtcp_control_loopback", Detail: err.Error()}}
	}
	mediaSender, err := net.DialUDP("udp", nil, mediaListener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		return []protocoltest.Check{{Name: "ps_rtp_media_loopback", Detail: err.Error()}, {Name: "rtcp_control_loopback", Detail: err.Error()}}
	}
	defer mediaSender.Close()
	rtcpSender, err := net.DialUDP("udp", nil, rtcpListener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		return []protocoltest.Check{{Name: "ps_rtp_media_loopback", Detail: err.Error()}, {Name: "rtcp_control_loopback", Detail: err.Error()}}
	}
	defer rtcpSender.Close()
	if _, err := mediaSender.Write(packed); err != nil {
		return []protocoltest.Check{{Name: "ps_rtp_media_loopback", Detail: err.Error()}, {Name: "rtcp_control_loopback", Detail: err.Error()}}
	}
	if _, err := rtcpSender.Write(rtcpData); err != nil {
		return []protocoltest.Check{{Name: "ps_rtp_media_loopback", Detail: err.Error()}, {Name: "rtcp_control_loopback", Detail: err.Error()}}
	}
	deadline := time.Now().Add(time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = mediaListener.SetReadDeadline(deadline)
	buffer := make([]byte, 64<<10)
	n, _, err := mediaListener.ReadFromUDP(buffer)
	if err != nil {
		return []protocoltest.Check{{Name: "ps_rtp_media_loopback", Detail: err.Error()}, {Name: "rtcp_control_loopback", Detail: err.Error()}}
	}
	demuxer := ps.NewDemuxer()
	frames, demuxErr := demuxer.Feed(buffer[:n])
	_ = rtcpListener.SetReadDeadline(deadline)
	rtcpBuffer := make([]byte, 1500)
	rtcpN, _, rtcpReadErr := rtcpListener.ReadFromUDP(rtcpBuffer)
	packets, rtcpErr := rtcp.Unmarshal(rtcpBuffer[:rtcpN])
	return []protocoltest.Check{
		{Name: "ps_rtp_media_loopback", Passed: demuxErr == nil && len(frames) > 0 && demuxer.VideoCodec() == avframe.CodecH264, Detail: fmt.Sprintf("PS frames=%d codec=%s received through localhost UDP", len(frames), demuxer.VideoCodec())},
		{Name: "rtcp_control_loopback", Passed: rtcpReadErr == nil && rtcpErr == nil && len(packets) == 1, Detail: "RTCP receiver report received by fake platform"},
	}
}

func gbUDPLoopbackCheck(ctx context.Context) protocoltest.Check {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return protocoltest.Check{Name: "rtp_udp_loopback", Detail: err.Error()}
	}
	defer listener.Close()
	packet := &pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: 96, SequenceNumber: 7, Timestamp: 90000, SSRC: 0x34020001}, Payload: []byte{0x00, 0x01, 0x02}}
	data, err := packet.Marshal()
	if err != nil {
		return protocoltest.Check{Name: "rtp_udp_loopback", Detail: err.Error()}
	}
	sender, err := net.DialUDP("udp", nil, listener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		return protocoltest.Check{Name: "rtp_udp_loopback", Detail: err.Error()}
	}
	defer sender.Close()
	if _, err := sender.Write(data); err != nil {
		return protocoltest.Check{Name: "rtp_udp_loopback", Detail: err.Error()}
	}
	deadline := time.Now().Add(time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = listener.SetReadDeadline(deadline)
	buf := make([]byte, 1500)
	n, _, err := listener.ReadFromUDP(buf)
	if err != nil {
		return protocoltest.Check{Name: "rtp_udp_loopback", Detail: err.Error()}
	}
	var decoded pionrtp.Packet
	if err := decoded.Unmarshal(buf[:n]); err != nil || string(decoded.Payload) != string(packet.Payload) {
		if err == nil {
			err = fmt.Errorf("payload mismatch")
		}
		return protocoltest.Check{Name: "rtp_udp_loopback", Detail: err.Error()}
	}
	return protocoltest.Check{Name: "rtp_udp_loopback", Passed: true, Detail: "RTP/PS transport returned through localhost UDP"}
}
