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

// playbackClient handles video recording playback sessions.
type playbackClient struct {
	sipService sipmod.SIPService
	handler    *handler
	sendInvite inviteSender
}

// playback starts a recording playback session from a device.
func (pc *playbackClient) playback(ctx context.Context, device *Device, channelID string, startTime, endTime time.Time, params map[string]string) (*MediaSession, error) {
	streamKey := fmt.Sprintf("%s/%s/playback", pc.handler.prefix, channelID)
	publishCtx := outboundPublishContext(device, streamKey, params)
	if err := authorizePublish(pc.handler.bus, publishCtx); err != nil {
		return nil, err
	}

	rtpPort, _, err := pc.handler.ports.AllocatePair()
	if err != nil {
		return nil, fmt.Errorf("allocate port pair: %w", err)
	}

	localIP := getLocalIP()
	// Build SDP offer with time range for playback
	sdpOffer := fmt.Sprintf(
		"v=0\r\no=- 0 0 IN IP4 %s\r\ns=Playback\r\nu=%s:0\r\nc=IN IP4 %s\r\nt=%d %d\r\nm=video %d RTP/AVP 96\r\na=recvonly\r\na=rtpmap:96 PS/90000\r\n",
		localIP, channelID, localIP,
		startTime.Unix(), endTime.Unix(),
		rtpPort,
	)

	serverID := pc.sipService.ServerID()
	domain := pc.sipService.Domain()
	remoteIP := extractIP(device.RemoteAddr)
	remotePort := parsePort(device.RemoteAddr)

	toURI := sip.Uri{
		User: channelID,
		Host: remoteIP,
		Port: remotePort,
	}

	req := sip.NewRequest(sip.INVITE, toURI)
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%s", serverID, domain, generateTag())))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", channelID, remoteIP)))
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.AppendHeader(sip.NewHeader("Subject", fmt.Sprintf("%s:0,%s:0", channelID, serverID)))
	req.SetBody([]byte(sdpOffer))

	_, streamExisted := pc.handler.hub.Find(streamKey)
	stream, err := pc.handler.hub.GetOrCreate(streamKey)
	if err != nil {
		pc.handler.ports.Free(rtpPort, rtpPort+1)
		return nil, fmt.Errorf("create playback stream: %w", err)
	}
	var pub *Publisher
	pub = NewPublisher(
		newPublisherID("playback", channelID),
		func(frame *avframe.AVFrame) {
			stream.WriteFrameForPublisher(pub, frame)
		},
	)
	receiver, err := newRTPReceiver(rtpPort, pub)
	if err != nil {
		_ = pub.Close()
		pc.handler.ports.Free(rtpPort, rtpPort+1)
		if !streamExisted {
			pc.handler.hub.Remove(streamKey)
		}
		return nil, fmt.Errorf("create playback RTP receiver: %w", err)
	}

	session := &MediaSession{
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
		Playback:  true,
	}

	// Send INVITE
	invTx, err := sendInvite(ctx, pc.sipService, pc.sendInvite, req)
	if err != nil {
		pc.handler.rollbackSession(session, !streamExisted)
		return nil, fmt.Errorf("send playback INVITE: %w", err)
	}
	defer invTx.Close()

	select {
	case <-invTx.Done():
	case <-time.After(10 * time.Second):
		pc.handler.rollbackSession(session, !streamExisted)
		return nil, fmt.Errorf("playback INVITE timeout")
	}

	resp := invTx.Response()
	if resp == nil {
		pc.handler.rollbackSession(session, !streamExisted)
		return nil, fmt.Errorf("playback INVITE: no response")
	}

	if resp.StatusCode != 200 {
		pc.handler.rollbackSession(session, !streamExisted)
		return nil, fmt.Errorf("playback INVITE rejected: %d %s", resp.StatusCode, resp.Reason)
	}

	if err := invTx.SendACK(ctx); err != nil {
		pc.handler.rollbackSession(session, !streamExisted)
		terminateAcceptedDialog(invTx)
		return nil, fmt.Errorf("send playback ACK: %w", err)
	}

	// Parse remote info from SDP answer
	if body := resp.Body(); len(body) > 0 {
		rPort := parseSDPPort(string(body))
		rIP := extractIP(device.RemoteAddr)
		session.RemoteAddr = &net.UDPAddr{IP: net.ParseIP(rIP), Port: rPort}
	}

	if callID := resp.CallID(); callID != nil {
		session.ID = callID.Value()
	}

	if err := stream.SetPublisher(pub); err != nil {
		terminateAcceptedDialog(invTx)
		pc.handler.rollbackSession(session, !streamExisted)
		return nil, fmt.Errorf("set playback publisher: %w", err)
	}
	session.SetState(SessionStateStreaming)
	pc.handler.sessions.Add(session)
	pc.handler.runReceiver(session, receiver)

	publishCtx.PublisherID = pub.ID()
	publishCtx.Extra = map[string]any{
		"gb28181_device_id":  device.DeviceID,
		"gb28181_channel_id": channelID,
		"gb28181_playback":   true,
	}
	session.startPublishLifecycle(func() {
		pc.handler.bus.EmitAsync(core.EventPublish, publishCtx)
	})

	slog.Info("playback started", "module", "gb28181",
		"device", device.DeviceID, "channel", channelID,
		"stream", streamKey, "start", startTime, "end", endTime)

	return session, nil
}
