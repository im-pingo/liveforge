package gb28181

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	sipmod "github.com/im-pingo/liveforge/module/sip"
)

// playbackClient handles video recording playback sessions.
type playbackClient struct {
	sipService sipmod.SIPService
	handler    *handler
}

// playback starts a recording playback session from a device.
func (pc *playbackClient) playback(ctx context.Context, device *Device, channelID string, startTime, endTime time.Time) (*MediaSession, error) {
	rtpPort, _, err := pc.handler.ports.AllocatePair()
	if err != nil {
		return nil, fmt.Errorf("allocate port pair: %w", err)
	}

	localIP := getLocalIP()
	streamKey := fmt.Sprintf("%s/%s/playback", pc.handler.prefix, channelID)

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

	// Create stream and publisher
	stream, _ := pc.handler.hub.GetOrCreate(streamKey)
	pub := NewPublisher(
		fmt.Sprintf("gb28181-playback-%s", channelID),
		func(frame *avframe.AVFrame) {
			stream.WriteFrame(frame)
		},
	)

	session := &MediaSession{
		DeviceID:  device.DeviceID,
		ChannelID: channelID,
		StreamKey: streamKey,
		Direction: SessionDirectionOutbound,
		LocalPort: rtpPort,
		Transport: device.Transport,
		State:     SessionStateInviting,
		Publisher: pub,
		Stream:    stream,
	}

	// Send INVITE
	invTx, err := pc.sipService.SendInvite(ctx, req)
	if err != nil {
		pc.handler.ports.Free(rtpPort, rtpPort+1)
		return nil, fmt.Errorf("send playback INVITE: %w", err)
	}

	select {
	case <-invTx.Done():
	case <-time.After(10 * time.Second):
		pc.handler.ports.Free(rtpPort, rtpPort+1)
		invTx.Close()
		return nil, fmt.Errorf("playback INVITE timeout")
	}

	resp := invTx.Response()
	if resp == nil {
		pc.handler.ports.Free(rtpPort, rtpPort+1)
		invTx.Close()
		return nil, fmt.Errorf("playback INVITE: no response")
	}

	if resp.StatusCode != 200 {
		pc.handler.ports.Free(rtpPort, rtpPort+1)
		invTx.Close()
		return nil, fmt.Errorf("playback INVITE rejected: %d %s", resp.StatusCode, resp.Reason)
	}

	if err := invTx.SendACK(ctx); err != nil {
		slog.Warn("failed to send ACK for playback", "module", "gb28181", "error", err)
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

	session.SetState(SessionStateStreaming)
	pc.handler.sessions.Add(session)

	// Start RTP receiver
	receiver, err := NewRTPReceiver(rtpPort, pub)
	if err != nil {
		pc.handler.ports.Free(rtpPort, rtpPort+1)
		return nil, fmt.Errorf("create playback RTP receiver: %w", err)
	}
	go receiver.Run()

	// Emit publish event
	pc.handler.bus.Emit(core.EventPublish, &core.EventContext{
		StreamKey:  streamKey,
		Protocol:   "gb28181",
		RemoteAddr: device.RemoteAddr,
		Extra: map[string]any{
			"gb28181_device_id":  device.DeviceID,
			"gb28181_channel_id": channelID,
			"gb28181_playback":   true,
		},
	})

	slog.Info("playback started", "module", "gb28181",
		"device", device.DeviceID, "channel", channelID,
		"stream", streamKey, "start", startTime, "end", endTime)

	return session, nil
}
