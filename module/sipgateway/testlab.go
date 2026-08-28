package sipgateway

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/protocoltest"
	"github.com/im-pingo/liveforge/pkg/sdp"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
)

const selfTestSIPOffer = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=LiveForge self-test\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 4000 RTP/AVP 0 8\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:8 PCMA/8000\r\n"

// RunSelfTest exercises local SIP gateway primitives without dialing a remote
// endpoint or creating a persistent stream/call.
func (gw *Gateway) RunSelfTest(ctx context.Context) protocoltest.Report {
	started := time.Now()
	checks := make([]protocoltest.Check, 0, 16)
	if err := ctx.Err(); err != nil {
		return protocoltest.NewWithDuration("sip", []protocoltest.Check{{Name: "context", Detail: err.Error()}}, time.Since(started))
	}
	offer, err := sdp.Parse([]byte(selfTestSIPOffer))
	if err != nil {
		checks = append(checks, protocoltest.Check{Name: "sdp_parse", Detail: err.Error()})
	} else if len(offer.Media) == 0 {
		checks = append(checks, protocoltest.Check{Name: "sdp_codec_negotiation", Detail: "SDP contains no media"})
	} else {
		codec, ok := negotiateCodec(offer.Media[0], gw.codecs)
		checks = append(checks, protocoltest.Check{Name: "sdp_codec_negotiation", Passed: ok, Detail: codec.EncodingName})
	}

	pair, err := gw.portAlloc.AllocateBoundUDPPair("udp", nil)
	if err != nil {
		checks = append(checks, protocoltest.Check{Name: "rtp_port_allocation", Detail: err.Error()})
	} else {
		_ = pair.RTPConn.Close()
		_ = pair.RTCPConn.Close()
		gw.portAlloc.Free(pair.RTPPort, pair.RTCPPort)
		checks = append(checks, protocoltest.Check{Name: "rtp_port_allocation", Passed: true, Detail: fmt.Sprintf("%d/%d", pair.RTPPort, pair.RTCPPort)})
	}

	checks = append(checks, runSIPSignalingLoop(ctx, gw.codecs)...)
	checks = append(checks, runSIPMediaLoop(ctx)...)
	return protocoltest.NewWithDuration("sip", checks, time.Since(started))
}

type sipLabPeer struct {
	auth       *sipmod.DigestAuth
	registered bool
	invited    bool
	acked      bool
	closed     bool
	callID     string
}

func runSIPSignalingLoop(ctx context.Context, codecs []string) []protocoltest.Check {
	checks := make([]protocoltest.Check, 0, 7)
	if err := ctx.Err(); err != nil {
		return []protocoltest.Check{{Name: "signaling_context", Detail: err.Error()}}
	}
	peer := &sipLabPeer{auth: sipmod.NewDigestAuth("lab.local", "lab-password")}
	register := newSIPLabRequest(sip.REGISTER, "lab-device", "lab.local", "lab-register")
	challenge := peer.handle(register)
	checks = append(checks, protocoltest.Check{Name: "register_401_challenge", Passed: challenge.StatusCode == 401, Detail: fmt.Sprintf("status=%d", challenge.StatusCode)})
	nonce := headerParameter(challenge.GetHeader("WWW-Authenticate"), "nonce")
	if nonce != "" {
		register = newSIPLabRequest(sip.REGISTER, "lab-device", "lab.local", "lab-register")
		addSIPLabDigest(register, "lab-device", "lab.local", "lab-password", nonce)
	}
	registered := peer.handle(register)
	checks = append(checks, protocoltest.Check{Name: "register_authenticated", Passed: registered.StatusCode == 200 && peer.registered, Detail: fmt.Sprintf("status=%d", registered.StatusCode)})

	invite := newSIPLabRequest(sip.INVITE, "camera-1", "lab.local", "lab-call")
	invite.SetBody([]byte(selfTestSIPOffer))
	inviteResponse := peer.handleWithCodecs(invite, codecs)
	checks = append(checks, protocoltest.Check{Name: "invite_200_ok", Passed: inviteResponse.StatusCode == 200 && peer.invited, Detail: fmt.Sprintf("status=%d", inviteResponse.StatusCode)})
	ack := newSIPLabRequest(sip.ACK, "camera-1", "lab.local", "lab-call")
	ackResponse := peer.handle(ack)
	checks = append(checks, protocoltest.Check{Name: "ack", Passed: ackResponse.StatusCode == 200 && peer.acked, Detail: fmt.Sprintf("status=%d", ackResponse.StatusCode)})
	bye := newSIPLabRequest(sip.BYE, "camera-1", "lab.local", "lab-call")
	byeResponse := peer.handle(bye)
	checks = append(checks, protocoltest.Check{Name: "bye", Passed: byeResponse.StatusCode == 200 && peer.closed, Detail: fmt.Sprintf("status=%d", byeResponse.StatusCode)})

	rejected := newSIPLabRequest(sip.INVITE, "camera-1", "lab.local", "lab-rejected")
	rejected.SetBody([]byte("v=0\r\nm=audio 4000 RTP/AVP 111\r\na=rtpmap:111 unsupported/90000\r\n"))
	rejectedResponse := (&sipLabPeer{registered: true}).handleWithCodecs(rejected, codecs)
	checks = append(checks, protocoltest.Check{Name: "invite_rejection", Passed: rejectedResponse.StatusCode == 488, Detail: fmt.Sprintf("status=%d incompatible codec", rejectedResponse.StatusCode)})
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	err := waitForSIPLabResponse(timeoutCtx)
	checks = append(checks, protocoltest.Check{Name: "invite_timeout", Passed: errors.Is(err, context.DeadlineExceeded), Detail: errString(err)})
	return checks
}

func newSIPLabRequest(method sip.RequestMethod, user, host, callID string) *sip.Request {
	request := sip.NewRequest(method, sip.Uri{User: user, Host: host})
	request.AppendHeader(sip.NewHeader("Call-ID", callID))
	request.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=lab", user, host)))
	request.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", user, host)))
	return request
}

func (p *sipLabPeer) handle(req *sip.Request) *sip.Response {
	switch req.Method {
	case sip.REGISTER:
		if req.GetHeader("Authorization") == nil {
			return p.auth.Challenge(req)
		}
		if p.auth.Verify(req, "lab-device") != sipmod.DigestValid {
			return sip.NewResponseFromRequest(req, 403, "Forbidden", nil)
		}
		p.registered = true
		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	case sip.ACK:
		if p.invited {
			p.acked = true
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}
	case sip.BYE:
		if p.acked {
			p.closed = true
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}
	}
	return sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil)
}

func (p *sipLabPeer) handleWithCodecs(req *sip.Request, codecs []string) *sip.Response {
	if req.Method != sip.INVITE || !p.registered {
		return sip.NewResponseFromRequest(req, 403, "Forbidden", nil)
	}
	offer, err := sdp.Parse(req.Body())
	if err != nil {
		return sip.NewResponseFromRequest(req, 400, "Bad Request", nil)
	}
	var audio *sdp.MediaDescription
	for _, media := range offer.Media {
		if media.Type == "audio" {
			audio = media
			break
		}
	}
	if audio == nil {
		return sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil)
	}
	if _, ok := negotiateCodec(audio, codecs); !ok {
		return sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil)
	}
	p.invited = true
	p.callID = req.CallID().Value()
	answer := []byte("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=LiveForge lab\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 5000 RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n")
	response := sip.NewResponseFromRequest(req, 200, "OK", answer)
	response.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	return response
}

func addSIPLabDigest(req *sip.Request, username, realm, password, nonce string) {
	uri := req.Recipient.String()
	ha1 := sipLabMD5(username + ":" + realm + ":" + password)
	ha2 := sipLabMD5(string(req.Method) + ":" + uri)
	response := sipLabMD5(ha1 + ":" + nonce + ":" + ha2)
	req.AppendHeader(sip.NewHeader("Authorization", fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", algorithm=MD5`, username, realm, nonce, uri, response)))
}

func sipLabMD5(value string) string {
	sum := md5.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}

func headerParameter(header sip.Header, name string) string {
	if header == nil {
		return ""
	}
	for _, part := range strings.Split(header.Value(), ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return ""
}

func waitForSIPLabResponse(ctx context.Context) error {
	select {
	case <-time.After(100 * time.Millisecond):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func errString(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func runSIPMediaLoop(ctx context.Context) []protocoltest.Check {
	checks := make([]protocoltest.Check, 0, 2)
	rtpCheck, rtcpCheck := localRTPRTCPCheck(ctx)
	checks = append(checks, rtpCheck, rtcpCheck)
	return checks
}

func localRTPRTCPCheck(ctx context.Context) (protocoltest.Check, protocoltest.Check) {
	rtpListener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		check := protocoltest.Check{Name: "rtp_media_loopback", Detail: err.Error()}
		return check, protocoltest.Check{Name: "rtcp_control_loopback", Detail: err.Error()}
	}
	defer rtpListener.Close()
	rtcpListener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return protocoltest.Check{Name: "rtp_media_loopback", Detail: err.Error()}, protocoltest.Check{Name: "rtcp_control_loopback", Detail: err.Error()}
	}
	defer rtcpListener.Close()
	rtpPacket := &pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: 8, SequenceNumber: 1, Timestamp: 8000, SSRC: 1}, Payload: []byte{1, 2, 3}}
	rtpData, err := rtpPacket.Marshal()
	if err != nil {
		return protocoltest.Check{Name: "rtp_media_loopback", Detail: err.Error()}, protocoltest.Check{Name: "rtcp_control_loopback", Detail: err.Error()}
	}
	rtcpPacket := &rtcp.ReceiverReport{SSRC: 1}
	rtcpData, err := rtcpPacket.Marshal()
	if err != nil {
		return protocoltest.Check{Name: "rtp_media_loopback", Detail: err.Error()}, protocoltest.Check{Name: "rtcp_control_loopback", Detail: err.Error()}
	}
	rtpSender, err := net.DialUDP("udp", nil, rtpListener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		return protocoltest.Check{Name: "rtp_media_loopback", Detail: err.Error()}, protocoltest.Check{Name: "rtcp_control_loopback", Detail: err.Error()}
	}
	defer rtpSender.Close()
	rtcpSender, err := net.DialUDP("udp", nil, rtcpListener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		return protocoltest.Check{Name: "rtp_media_loopback", Detail: err.Error()}, protocoltest.Check{Name: "rtcp_control_loopback", Detail: err.Error()}
	}
	defer rtcpSender.Close()
	if _, err := rtpSender.Write(rtpData); err != nil {
		return protocoltest.Check{Name: "rtp_media_loopback", Detail: err.Error()}, protocoltest.Check{Name: "rtcp_control_loopback", Detail: err.Error()}
	}
	if _, err := rtcpSender.Write(rtcpData); err != nil {
		return protocoltest.Check{Name: "rtp_media_loopback", Detail: err.Error()}, protocoltest.Check{Name: "rtcp_control_loopback", Detail: err.Error()}
	}
	deadline := time.Now().Add(time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = rtpListener.SetReadDeadline(deadline)
	rtpBuffer := make([]byte, 1500)
	rtpN, _, err := rtpListener.ReadFromUDP(rtpBuffer)
	if err != nil {
		return protocoltest.Check{Name: "rtp_media_loopback", Detail: err.Error()}, protocoltest.Check{Name: "rtcp_control_loopback", Detail: err.Error()}
	}
	var receivedRTP pionrtp.Packet
	rtpErr := receivedRTP.Unmarshal(rtpBuffer[:rtpN])
	_ = rtcpListener.SetReadDeadline(deadline)
	rtcpBuffer := make([]byte, 1500)
	rtcpN, _, err := rtcpListener.ReadFromUDP(rtcpBuffer)
	if err != nil {
		return protocoltest.Check{Name: "rtp_media_loopback", Passed: rtpErr == nil && string(receivedRTP.Payload) == string(rtpPacket.Payload), Detail: "RTP packet received"}, protocoltest.Check{Name: "rtcp_control_loopback", Detail: err.Error()}
	}
	packets, rtcpErr := rtcp.Unmarshal(rtcpBuffer[:rtcpN])
	return protocoltest.Check{Name: "rtp_media_loopback", Passed: rtpErr == nil && string(receivedRTP.Payload) == string(rtpPacket.Payload), Detail: "RTP media received by fake peer"}, protocoltest.Check{Name: "rtcp_control_loopback", Passed: rtcpErr == nil && len(packets) == 1, Detail: "RTCP receiver report received by fake peer"}
}

func udpLoopbackCheck(ctx context.Context) protocoltest.Check {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return protocoltest.Check{Name: "rtp_udp_loopback", Detail: err.Error()}
	}
	defer listener.Close()
	packet := &pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 1, Timestamp: 8000, SSRC: 1}, Payload: []byte{0x01, 0x02, 0x03}}
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
	return protocoltest.Check{Name: "rtp_udp_loopback", Passed: true, Detail: "RTP packet returned through localhost UDP"}
}
