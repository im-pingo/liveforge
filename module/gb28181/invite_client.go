package gb28181

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

// inviteClient sends INVITE requests to GB28181 devices for live play or playback.
type inviteClient struct {
	sipService sipmod.SIPService
	handler    *handler
	sendInvite inviteSender
}

// invite sends an INVITE to a device channel and sets up the media session.
func (ic *inviteClient) invite(ctx context.Context, device *Device, channelID string, params map[string]string) (*MediaSession, error) {
	streamKey := fmt.Sprintf("%s/%s", ic.handler.prefix, channelID)
	publishCtx := outboundPublishContext(device, streamKey, params)
	if err := authorizePublish(ic.handler.bus, publishCtx); err != nil {
		return nil, err
	}

	// Allocate local RTP port pair
	rtpPort, _, err := ic.handler.ports.AllocatePair()
	if err != nil {
		return nil, fmt.Errorf("allocate port pair: %w", err)
	}

	localIP := getLocalIP()
	// Build SDP offer
	sdpOffer := fmt.Sprintf(
		"v=0\r\no=- 0 0 IN IP4 %s\r\ns=LiveForge\r\nc=IN IP4 %s\r\nt=0 0\r\nm=video %d RTP/AVP 96\r\na=recvonly\r\na=rtpmap:96 PS/90000\r\n",
		localIP, localIP, rtpPort,
	)

	// Build SIP INVITE request
	serverID := ic.sipService.ServerID()
	domain := ic.sipService.Domain()
	remoteAddr := device.RemoteAddr

	toURI := sip.Uri{
		User: channelID,
		Host: extractIP(remoteAddr),
		Port: parsePort(remoteAddr),
	}

	fromURI := sip.Uri{
		User: serverID,
		Host: domain,
	}

	req := sip.NewRequest(sip.INVITE, toURI)
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%s", fromURI.User, fromURI.Host, generateTag())))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", toURI.User, toURI.Host)))
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.AppendHeader(sip.NewHeader("Subject", fmt.Sprintf("%s:0,%s:0", channelID, serverID)))
	req.SetBody([]byte(sdpOffer))

	_, streamExisted := ic.handler.hub.Find(streamKey)
	stream, err := ic.handler.hub.GetOrCreate(streamKey)
	if err != nil {
		ic.handler.ports.Free(rtpPort, rtpPort+1)
		return nil, fmt.Errorf("create stream: %w", err)
	}
	pub := NewPublisher(
		newPublisherID("live", channelID),
		func(frame *avframe.AVFrame) {
			stream.WriteFrame(frame)
		},
	)
	receiver, err := newRTPReceiver(rtpPort, pub)
	if err != nil {
		_ = pub.Close()
		ic.handler.ports.Free(rtpPort, rtpPort+1)
		if !streamExisted {
			ic.handler.hub.Remove(streamKey)
		}
		return nil, fmt.Errorf("create RTP receiver: %w", err)
	}

	// Create media session
	session := &MediaSession{
		ID:        "", // set after INVITE response
		DeviceID:  device.DeviceID,
		ChannelID: channelID,
		StreamKey: streamKey,
		Direction: SessionDirectionOutbound,
		LocalPort: rtpPort,
		Transport: device.Transport,
		State:     SessionStateInviting,
		Publisher: pub,
		Receiver:  receiver,
		Stream:    stream,
	}

	// Send INVITE
	invTx, err := sendInvite(ctx, ic.sipService, ic.sendInvite, req)
	if err != nil {
		ic.handler.rollbackSession(session, !streamExisted)
		return nil, fmt.Errorf("send INVITE: %w", err)
	}
	defer invTx.Close()

	// Wait for final response
	select {
	case <-invTx.Done():
	case <-time.After(10 * time.Second):
		ic.handler.rollbackSession(session, !streamExisted)
		return nil, fmt.Errorf("INVITE timeout")
	}

	resp := invTx.Response()
	if resp == nil {
		ic.handler.rollbackSession(session, !streamExisted)
		return nil, fmt.Errorf("INVITE: no response")
	}

	if resp.StatusCode != 200 {
		ic.handler.rollbackSession(session, !streamExisted)
		return nil, fmt.Errorf("INVITE rejected: %d %s", resp.StatusCode, resp.Reason)
	}

	// Send ACK
	if err := invTx.SendACK(ctx); err != nil {
		ic.handler.rollbackSession(session, !streamExisted)
		terminateAcceptedDialog(invTx)
		return nil, fmt.Errorf("send ACK: %w", err)
	}

	// Parse remote RTP port from SDP answer
	if body := resp.Body(); len(body) > 0 {
		remotePort := parseSDPPort(string(body))
		remoteIP := extractIP(device.RemoteAddr)
		session.RemoteAddr = &net.UDPAddr{IP: net.ParseIP(remoteIP), Port: remotePort}
	}

	// Set Call-ID from the INVITE transaction
	if callID := resp.CallID(); callID != nil {
		session.ID = callID.Value()
	}

	if err := stream.SetPublisher(pub); err != nil {
		terminateAcceptedDialog(invTx)
		ic.handler.rollbackSession(session, !streamExisted)
		return nil, fmt.Errorf("set publisher: %w", err)
	}
	session.SetState(SessionStateStreaming)
	ic.handler.sessions.Add(session)
	go receiver.Run()

	publishCtx.PublisherID = pub.ID()
	publishCtx.Extra = map[string]any{
		"gb28181_device_id":  device.DeviceID,
		"gb28181_channel_id": channelID,
	}
	session.startPublishLifecycle(func() {
		ic.handler.bus.EmitAsync(core.EventPublish, publishCtx)
	})

	slog.Info("outbound invite accepted", "module", "gb28181",
		"device", device.DeviceID, "channel", channelID,
		"stream", streamKey, "local_port", rtpPort)

	return session, nil
}

// parsePort extracts the port number from a host:port string.
func parsePort(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 5060
	}
	port := 0
	fmt.Sscanf(portStr, "%d", &port)
	if port == 0 {
		return 5060
	}
	return port
}

// generateTag creates a simple SIP tag value.
func generateTag() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1000000000)
}
