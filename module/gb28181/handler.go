package gb28181

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/portalloc"
)

// handler processes SIP requests for the GB28181 module.
type handler struct {
	server   *core.Server
	registry *DeviceRegistry
	sessions *SessionManager
	hub      *core.StreamHub
	bus      *core.EventBus
	ports    *portalloc.PortAllocator
	prefix   string
	alarm    *alarmHandler
	auth     *sipmod.DigestAuth
	authMu   sync.RWMutex
}

func (h *handler) handleRegister(req *sip.Request, tx sip.ServerTransaction) {
	from := req.From()
	if from == nil {
		resp := sip.NewResponseFromRequest(req, 400, "Bad Request", nil)
		tx.Respond(resp)
		return
	}

	deviceID := from.Address.User
	if !h.authenticate(req, tx) {
		return
	}
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

func (h *handler) digestAuth() *sipmod.DigestAuth {
	h.authMu.RLock()
	auth := h.auth
	h.authMu.RUnlock()
	return auth
}

func (h *handler) setDigestAuth(cfg config.SIPConfig) {
	var auth *sipmod.DigestAuth
	if cfg.Auth.Enabled {
		auth = sipmod.NewDigestAuth(cfg.Domain, cfg.Auth.Password)
	}
	h.authMu.Lock()
	h.auth = auth
	h.authMu.Unlock()
}

func (h *handler) authenticate(req *sip.Request, tx sip.ServerTransaction) bool {
	auth := h.digestAuth()
	if auth == nil || auth.Verify(req) {
		return true
	}
	_ = tx.Respond(auth.Challenge(req))
	return false
}

func (h *handler) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
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
	streamKey := fmt.Sprintf("%s/%s", h.prefix, channelID)
	body := req.Body()
	if len(body) == 0 {
		resp := sip.NewResponseFromRequest(req, 400, "Missing SDP", nil)
		tx.Respond(resp)
		return
	}
	if !strings.Contains(strings.ToLower(string(body)), "m=video") {
		return
	}
	if !h.authenticate(req, tx) {
		return
	}
	if err := h.authorizePublish(core.AuthorizationPreSession, streamKey, req, deviceID, channelID); err != nil {
		tx.Respond(sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)) //nolint:errcheck
		return
	}

	remotePort := parseSDPPort(string(body))
	remoteIP := extractIP(req.Source())
	if err := h.authorizePublish(core.AuthorizationPostConnect, streamKey, req, deviceID, channelID); err != nil {
		tx.Respond(sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)) //nolint:errcheck
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

	// Create or get stream
	_, streamExisted := h.hub.Find(streamKey)
	stream, err := h.hub.GetOrCreate(streamKey)
	if err != nil {
		h.ports.Free(rtpPort, rtpPort+1)
		tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil)) //nolint:errcheck
		return
	}

	// Create publisher
	pub := NewPublisher(
		fmt.Sprintf("gb28181-%s", channelID),
		func(frame *avframe.AVFrame) {
			stream.WriteFrame(frame)
		},
	)

	// Bind the media socket before committing publisher/session state.
	receiver, err := NewRTPReceiver(rtpPort, pub)
	if err != nil {
		slog.Error("rtp receiver creation failed", "module", "gb28181", "error", err)
		h.ports.Free(rtpPort, rtpPort+1)
		if !streamExisted {
			h.hub.Remove(streamKey)
		}
		tx.Respond(sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil)) //nolint:errcheck
		return
	}

	generation, err := stream.SetPublisherWithGeneration(pub)
	if err != nil {
		receiver.Close()
		h.ports.Free(rtpPort, rtpPort+1)
		if !streamExisted {
			h.hub.Remove(streamKey)
		}
		slog.Warn("set publisher failed", "module", "gb28181", "error", err)
		tx.Respond(sip.NewResponseFromRequest(req, 409, "Conflict", nil)) //nolint:errcheck
		return
	}

	// Create media session
	session := &MediaSession{
		ID:                  getCallID(req),
		DeviceID:            deviceID,
		ChannelID:           channelID,
		StreamKey:           streamKey,
		Direction:           SessionDirectionInbound,
		LocalPort:           rtpPort,
		RemoteAddr:          &net.UDPAddr{IP: net.ParseIP(remoteIP), Port: remotePort},
		Transport:           "udp",
		State:               SessionStateStreaming,
		Publisher:           pub,
		PublisherGeneration: generation,
		Receiver:            receiver,
		Stream:              stream,
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
	tx.Respond(resp)

	// Emit publish event
	h.bus.Emit(core.EventPublish, &core.EventContext{
		StreamKey:  streamKey,
		Protocol:   "gb28181",
		RemoteAddr: req.Source(),
		Extra: map[string]any{
			"gb28181_device_id":  deviceID,
			"gb28181_channel_id": channelID,
		},
	})

	slog.Info("invite accepted", "module", "gb28181",
		"device", deviceID, "channel", channelID,
		"stream", streamKey, "local_port", rtpPort)
}

func (h *handler) authorizePublish(stage core.AuthorizationStage, streamKey string, req *sip.Request, deviceID, channelID string) error {
	if h.server == nil {
		return nil
	}
	return h.server.Authorize(context.Background(), core.AuthorizationRequest{
		Action: core.AuthorizationPublish, Stage: stage, StreamKey: streamKey,
		Protocol: "gb28181", RemoteAddr: req.Source(), Params: sipRequestParams(req),
		Extra: map[string]any{
			"gb28181_device_id": deviceID, "gb28181_channel_id": channelID,
		},
	})
}

func (h *handler) handleBye(req *sip.Request, tx sip.ServerTransaction) {
	if !h.authenticate(req, tx) {
		return
	}
	callID := getCallID(req)
	session := h.sessions.Take(callID)
	if session != nil {
		h.cleanupSession(session, req.Source())

		slog.Info("session closed by BYE", "module", "gb28181",
			"session", callID, "stream", session.StreamKey)
	}

	resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
	tx.Respond(resp)
}

func (h *handler) handleMessage(req *sip.Request, tx sip.ServerTransaction) {
	if !h.authenticate(req, tx) {
		return
	}
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

func (h *handler) handleSubscribe(req *sip.Request, tx sip.ServerTransaction) {
	if !h.authenticate(req, tx) {
		return
	}
	if h.alarm != nil {
		h.alarm.handleSubscribe(req, tx)
		return
	}
	tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil)) //nolint:errcheck
}

func (h *handler) cleanupSession(session *MediaSession, remoteAddr string) {
	if session == nil {
		return
	}
	session.Close()
	removed := false
	if session.Stream != nil {
		removed = session.Stream.RemovePublisherIfGeneration(session.PublisherGeneration)
	}
	if h.ports != nil {
		h.ports.Free(session.LocalPort, session.LocalPort+1)
	}
	if removed && h.bus != nil {
		h.bus.Emit(core.EventPublishStop, &core.EventContext{
			StreamKey:  session.StreamKey,
			Protocol:   "gb28181",
			RemoteAddr: remoteAddr,
			Extra: map[string]any{
				"gb28181_device_id":  session.DeviceID,
				"gb28181_channel_id": session.ChannelID,
			},
		}) //nolint:errcheck
	}
}

func sipRequestParams(req *sip.Request) map[string]string {
	params := make(map[string]string)
	for key, value := range req.Recipient.UriParams.Items() {
		params[key] = value
	}
	for key, value := range req.Recipient.Headers.Items() {
		params[key] = value
	}
	if header := req.GetHeader("Authorization"); header != nil {
		value := header.Value()
		params["authorization"] = value
		if strings.HasPrefix(strings.ToLower(value), "bearer ") {
			params["token"] = strings.TrimSpace(value[len("Bearer "):])
		}
	}
	return params
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
