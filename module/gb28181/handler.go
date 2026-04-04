package gb28181

import (
	"encoding/xml"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/portalloc"
)

// handler processes SIP requests for the GB28181 module.
type handler struct {
	registry *DeviceRegistry
	sessions *SessionManager
	hub      *core.StreamHub
	bus      *core.EventBus
	ports    *portalloc.PortAllocator
	prefix   string
	auth     *digestAuthConfig
}

type digestAuthConfig struct {
	enabled  bool
	realm    string
	password string
}

func (h *handler) handleRegister(req *sip.Request, tx sip.ServerTransaction) {
	from := req.From()
	if from == nil {
		resp := sip.NewResponseFromRequest(req, 400, "Bad Request", nil)
		tx.Respond(resp)
		return
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

	// Allocate local RTP port pair
	rtpPort, _, err := h.ports.AllocatePair()
	if err != nil {
		slog.Error("port allocation failed", "module", "gb28181", "error", err)
		resp := sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil)
		tx.Respond(resp)
		return
	}

	streamKey := fmt.Sprintf("%s/%s", h.prefix, channelID)

	// Create or get stream
	stream, _ := h.hub.GetOrCreate(streamKey)

	// Create publisher
	pub := NewPublisher(
		fmt.Sprintf("gb28181-%s", channelID),
		func(frame *avframe.AVFrame) {
			stream.WriteFrame(frame)
		},
	)

	// Set publisher on stream so subscribers can connect
	if err := stream.SetPublisher(pub); err != nil {
		slog.Warn("set publisher failed", "module", "gb28181", "error", err)
	}

	// Create media session
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
		Stream:     stream,
	}
	h.sessions.Add(session)

	// Create RTP receiver
	receiver, err := NewRTPReceiver(rtpPort, pub)
	if err != nil {
		slog.Error("rtp receiver creation failed", "module", "gb28181", "error", err)
		h.ports.Free(rtpPort, rtpPort+1)
		resp := sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil)
		tx.Respond(resp)
		return
	}
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

func (h *handler) handleBye(req *sip.Request, tx sip.ServerTransaction) {
	callID := getCallID(req)
	session := h.sessions.Get(callID)
	if session != nil {
		session.Close()
		h.sessions.Remove(callID)

		if session.Stream != nil {
			session.Stream.RemovePublisher()
		}

		h.ports.Free(session.LocalPort, session.LocalPort+1)

		h.bus.Emit(core.EventPublishStop, &core.EventContext{
			StreamKey:  session.StreamKey,
			Protocol:   "gb28181",
			RemoteAddr: req.Source(),
			Extra: map[string]any{
				"gb28181_device_id":  session.DeviceID,
				"gb28181_channel_id": session.ChannelID,
			},
		})

		slog.Info("session closed by BYE", "module", "gb28181",
			"session", callID, "stream", session.StreamKey)
	}

	resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
	tx.Respond(resp)
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
