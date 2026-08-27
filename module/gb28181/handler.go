package gb28181

import (
	"encoding/xml"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/portalloc"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

// handler processes SIP requests for the GB28181 module.
type handler struct {
	registry *DeviceRegistry
	sessions *SessionManager
	hub      *core.StreamHub
	bus      *core.EventBus
	ports    *portalloc.PortAllocator
	prefix   string
	auth     *sipmod.DigestAuth
}

const labStreamKeyHeader = "X-LiveForge-Lab-Stream-Key"

func (h *handler) handleRegister(req *sip.Request, tx sip.ServerTransaction) {
	from := req.From()
	if from == nil {
		resp := sip.NewResponseFromRequest(req, 400, "Bad Request", nil)
		tx.Respond(resp)
		return
	}
	if h.auth != nil {
		switch h.auth.Verify(req, from.Address.User) {
		case sipmod.DigestValid:
		case sipmod.DigestStale:
			_ = tx.Respond(h.auth.Challenge(req, true))
			return
		default:
			if req.GetHeader("Authorization") == nil {
				_ = tx.Respond(h.auth.Challenge(req))
			} else {
				_ = tx.Respond(sip.NewResponseFromRequest(req, 403, "Forbidden", nil))
			}
			return
		}
	}

	deviceID := from.Address.User
	remoteAddr := req.Source()
	transport := "udp"
	if via := req.Via(); via != nil {
		transport = strings.ToLower(via.Transport)
	}

	// Check for Expires: 0 (unregister)
	expires := req.GetHeader("Expires")
	if expires != nil && expires.Value() == "0" {
		h.registry.Unregister(deviceID)
		resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(resp)
		return
	}

	h.registry.Register(deviceID, remoteAddr, transport)

	resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
	resp.AppendHeader(sip.NewHeader("Expires", "3600"))
	tx.Respond(resp)
}

func (h *handler) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	// SIP and GB28181 share one transport. Only claim video PS offers; audio
	// INVITEs must remain available for the SIP gateway handler registered after
	// this module.
	if !isGB28181VideoInvite(req) {
		return
	}

	from := req.From()
	if from == nil {
		resp := sip.NewResponseFromRequest(req, 400, "Bad Request", nil)
		tx.Respond(resp)
		return
	}

	deviceID := from.Address.User
	to := req.To()
	if to == nil {
		resp := sip.NewResponseFromRequest(req, 400, "Bad Request", nil)
		tx.Respond(resp)
		return
	}
	channelID := to.Address.User

	// Parse SDP from INVITE body to get remote RTP info
	body := req.Body()
	if len(body) == 0 {
		resp := sip.NewResponseFromRequest(req, 400, "Missing SDP", nil)
		tx.Respond(resp)
		return
	}

	remotePort := parseSDPPort(string(body))
	remoteIP := extractIP(req.Source())
	streamKey := inboundStreamKey(req, h.prefix, channelID)
	publishCtx := sipPublishContext(req, streamKey, "")
	if err := h.bus.EmitSync(core.EventPublish, publishCtx); err != nil {
		resp := sip.NewResponseFromRequest(req, 403, "Forbidden", nil)
		tx.Respond(resp)
		return
	}

	// Allocate local RTP port pair
	rtpPort, _, err := h.ports.AllocatePair()
	if err != nil {
		slog.Error("port allocation failed", "module", "gb28181", "error", err)
		resp := sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil)
		tx.Respond(resp)
		return
	}

	// Create or get stream.
	_, streamExisted := h.hub.Find(streamKey)
	stream, err := h.hub.GetOrCreate(streamKey)
	if err != nil {
		h.ports.Free(rtpPort, rtpPort+1)
		resp := sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil)
		tx.Respond(resp)
		return
	}

	// Create publisher
	pub := NewPublisher(
		newPublisherID("live", channelID),
		func(frame *avframe.AVFrame) {
			stream.WriteFrame(frame)
		},
	)

	receiver, err := newRTPReceiver(rtpPort, pub)
	if err != nil {
		slog.Error("rtp receiver creation failed", "module", "gb28181", "error", err)
		_ = pub.Close()
		h.ports.Free(rtpPort, rtpPort+1)
		if !streamExisted {
			h.hub.Remove(streamKey)
		}
		resp := sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil)
		tx.Respond(resp)
		return
	}

	session := &MediaSession{
		ID:         getCallID(req),
		DeviceID:   deviceID,
		ChannelID:  channelID,
		StreamKey:  streamKey,
		Direction:  SessionDirectionInbound,
		LocalPort:  rtpPort,
		RemoteAddr: &net.UDPAddr{IP: net.ParseIP(remoteIP), Port: remotePort},
		Transport:  "udp",
		State:      SessionStateStreaming,
		Publisher:  pub,
		Receiver:   receiver,
		Stream:     stream,
	}

	if err := stream.SetPublisher(pub); err != nil {
		slog.Warn("set publisher failed", "module", "gb28181", "error", err)
		session.Close()
		h.ports.Free(rtpPort, rtpPort+1)
		if !streamExisted {
			h.hub.Remove(streamKey)
		}
		resp := sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil)
		tx.Respond(resp)
		return
	}
	h.sessions.Add(session)
	go receiver.Run()

	// Build SDP answer
	localIP := getLocalIP()
	sdpAnswer := fmt.Sprintf(
		"v=0\r\no=- 0 0 IN IP4 %s\r\ns=LiveForge\r\nc=IN IP4 %s\r\nt=0 0\r\nm=video %d RTP/AVP 96\r\na=recvonly\r\na=rtpmap:96 PS/90000\r\n",
		localIP, localIP, rtpPort,
	)

	resp := sip.NewResponseFromRequest(req, 200, "OK", []byte(sdpAnswer))
	resp.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	if err := tx.Respond(resp); err != nil {
		slog.Warn("failed to send INVITE final response", "module", "gb28181", "error", err)
		h.rollbackSession(session, !streamExisted)
		return
	}

	// Marking and enqueueing are one session-owned transition so teardown
	// cannot overtake a publish start that has not reached the EventBus yet.
	publishCtx.PublisherID = pub.ID()
	publishCtx.Extra = map[string]any{
		"gb28181_device_id":  deviceID,
		"gb28181_channel_id": channelID,
	}
	session.startPublishLifecycle(func() {
		h.bus.EmitAsync(core.EventPublish, publishCtx)
	})

	slog.Info("invite accepted", "module", "gb28181",
		"device", deviceID, "channel", channelID,
		"stream", streamKey, "local_port", rtpPort)
}

func isGB28181VideoInvite(req *sip.Request) bool {
	if req == nil || len(req.Body()) == 0 {
		return false
	}
	offer, err := sdp.Parse(req.Body())
	if err != nil {
		return false
	}
	for _, media := range offer.Media {
		if media == nil || !strings.EqualFold(media.Type, "video") || !strings.EqualFold(media.Proto, "RTP/AVP") {
			continue
		}
		for _, payloadType := range media.Formats {
			if payloadType != 96 {
				continue
			}
			mapping := media.RTPMap(payloadType)
			// Some older devices omit rtpmap for the GB28181 static lab-style
			// offer. Keep accepting that established shape while rejecting an
			// explicitly announced non-PS video codec.
			if mapping == nil || (strings.EqualFold(mapping.EncodingName, "PS") && mapping.ClockRate == 90000) {
				return true
			}
		}
	}
	return false
}

func inboundStreamKey(req *sip.Request, prefix, channelID string) string {
	defaultKey := fmt.Sprintf("%s/%s", prefix, channelID)
	if req == nil || !isLoopbackSIPSource(req.Source()) {
		return defaultKey
	}
	header := req.GetHeader(labStreamKeyHeader)
	if header == nil || !validGBLabStreamKey(header.Value()) {
		return defaultKey
	}
	return header.Value()
}

func isLoopbackSIPSource(source string) bool {
	host := source
	if parsedHost, _, err := net.SplitHostPort(source); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (h *handler) handleBye(req *sip.Request, tx sip.ServerTransaction) {
	callID := getCallID(req)
	session := h.sessions.Get(callID)
	if session != nil {
		snapshot := session.Snapshot()
		h.closeSession(session, req.Source())

		slog.Info("session closed by BYE", "module", "gb28181",
			"session", callID, "stream", snapshot.StreamKey)
	}

	resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
	tx.Respond(resp)
}

func (h *handler) closeSession(session *MediaSession, remoteAddr string) bool {
	if session == nil {
		return false
	}
	snapshot, closed := session.closeSnapshot()
	if !closed {
		return false
	}
	if snapshot.LocalPort > 0 && h.ports != nil {
		h.ports.Free(snapshot.LocalPort, snapshot.LocalPort+1)
	}
	if snapshot.Stream != nil && snapshot.Publisher != nil {
		snapshot.Stream.RemovePublisherIf(snapshot.Publisher)
	}
	if snapshot.ID != "" && h.sessions != nil {
		h.sessions.RemoveIf(snapshot.ID, session)
	}
	if snapshot.Published {
		if remoteAddr == "" && snapshot.RemoteAddr != nil {
			remoteAddr = snapshot.RemoteAddr.String()
		}
		h.bus.EmitAsync(core.EventPublishStop, &core.EventContext{
			StreamKey:   snapshot.StreamKey,
			PublisherID: snapshot.PublisherID,
			Protocol:    "gb28181",
			RemoteAddr:  remoteAddr,
			Extra: map[string]any{
				"gb28181_device_id":  snapshot.DeviceID,
				"gb28181_channel_id": snapshot.ChannelID,
				"gb28181_playback":   snapshot.Playback,
			},
		})
	}
	return true
}

func (h *handler) rollbackSession(session *MediaSession, removeStream bool) {
	snapshot := session.Snapshot()
	if !h.closeSession(session, "") {
		return
	}
	if removeStream && snapshot.Stream != nil && snapshot.Stream.Publisher() == nil {
		h.hub.Remove(snapshot.StreamKey)
	}
}

func (h *handler) closeSessionsByDevice(deviceID string) {
	for _, session := range h.sessions.All() {
		if session.Snapshot().DeviceID == deviceID {
			h.closeSession(session, "")
		}
	}
}

func (h *handler) closeSessionsByChannel(channelID string) int {
	closed := 0
	for _, session := range h.sessions.GetByChannel(channelID) {
		if h.closeSession(session, "") {
			closed++
		}
	}
	return closed
}

func (h *handler) handleMessage(req *sip.Request, tx sip.ServerTransaction) {
	body := req.Body()
	if len(body) == 0 {
		resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(resp)
		return
	}

	cmdType := ParseMessageType(body)
	switch cmdType {
	case "Keepalive":
		msg, err := ParseKeepalive(body)
		if err == nil {
			h.registry.Keepalive(msg.DeviceID)
		}
	case "Catalog":
		resp, err := ParseCatalogResponse(body)
		if err == nil {
			h.handleCatalogResponse(resp)
		}
	case "Alarm":
		alarm, err := ParseAlarmNotify(body)
		if err == nil {
			slog.Info("alarm received", "module", "gb28181",
				"device", alarm.DeviceID, "method", alarm.AlarmMethod,
				"type", alarm.AlarmType)
		}
	default:
		slog.Debug("unhandled message type", "module", "gb28181", "type", cmdType)
	}

	resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
	tx.Respond(resp)
}

func (h *handler) handleCatalogResponse(resp *CatalogResponse) {
	channels := make(map[string]*Channel)
	for _, item := range resp.DeviceList.Items {
		channels[item.DeviceID] = &Channel{
			ChannelID:    item.DeviceID,
			Name:         item.Name,
			Manufacturer: item.Manufacturer,
			Status:       item.Status,
			PTZType:      item.PTZType,
			Latitude:     item.Latitude,
			Longitude:    item.Longitude,
		}
	}
	h.registry.UpdateChannels(resp.DeviceID, channels)
}

// --- Helpers ---

func getCallID(req *sip.Request) string {
	if h := req.CallID(); h != nil {
		return h.Value()
	}
	return ""
}

func extractIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func parseSDPPort(sdp string) int {
	// Simple parser: find "m=video <port>" or "m=audio <port>"
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "m=") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				port := 0
				fmt.Sscanf(parts[1], "%d", &port)
				return port
			}
		}
	}
	return 0
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "0.0.0.0"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "0.0.0.0"
}

// XMLResponse is a minimal wrapper for checking XML root element name.
type XMLResponse struct {
	XMLName xml.Name
}
