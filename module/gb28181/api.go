package gb28181

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
)

const apiPrefix = "/api/v1/gb28181"

// registerAPI registers GB28181 HTTP API endpoints on the server.
func registerAPI(s *core.Server, m *Module) {
	// Devices
	s.RegisterAPIHandler("GET "+apiPrefix+"/devices", http.HandlerFunc(m.apiListDevices))
	s.RegisterAPIHandler("GET "+apiPrefix+"/devices/", http.HandlerFunc(m.apiDeviceDetail))
	s.RegisterAPIHandler("DELETE "+apiPrefix+"/devices/", http.HandlerFunc(m.apiDeleteDevice))

	// Channels
	s.RegisterAPIHandler("GET "+apiPrefix+"/channels", http.HandlerFunc(m.apiListChannels))
	s.RegisterAPIHandler("POST "+apiPrefix+"/channels/", http.HandlerFunc(m.apiChannelAction))

	// Sessions
	s.RegisterAPIHandler("GET "+apiPrefix+"/sessions", http.HandlerFunc(m.apiListSessions))
	s.RegisterAPIHandler("DELETE "+apiPrefix+"/sessions/", http.HandlerFunc(m.apiDeleteSession))
}

// extractPathParam extracts the remaining path after the given prefix.
func extractPathParam(path, prefix string) string {
	return strings.TrimPrefix(path, prefix)
}

// --- Device endpoints ---

func (m *Module) apiListDevices(w http.ResponseWriter, r *http.Request) {
	devices := m.registry.All()
	type deviceDTO struct {
		DeviceID      string    `json:"device_id"`
		RemoteAddr    string    `json:"remote_addr"`
		Transport     string    `json:"transport"`
		Status        string    `json:"status"`
		RegisteredAt  time.Time `json:"registered_at"`
		LastKeepalive time.Time `json:"last_keepalive"`
		ChannelCount  int       `json:"channel_count"`
	}
	out := make([]deviceDTO, 0, len(devices))
	for _, d := range devices {
		out = append(out, deviceDTO{
			DeviceID:      d.DeviceID,
			RemoteAddr:    d.RemoteAddr,
			Transport:     d.Transport,
			Status:        d.Status.String(),
			RegisteredAt:  d.RegisteredAt,
			LastKeepalive: d.LastKeepalive,
			ChannelCount:  len(d.Channels),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (m *Module) apiDeviceDetail(w http.ResponseWriter, r *http.Request) {
	deviceID := extractPathParam(r.URL.Path, apiPrefix+"/devices/")
	if deviceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing device_id"})
		return
	}

	dev := m.registry.Get(deviceID)
	if dev == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}

	type channelDTO struct {
		ChannelID    string  `json:"channel_id"`
		Name         string  `json:"name"`
		Manufacturer string  `json:"manufacturer"`
		Status       string  `json:"status"`
		PTZType      int     `json:"ptz_type"`
		Latitude     float64 `json:"latitude"`
		Longitude    float64 `json:"longitude"`
	}
	channels := make([]channelDTO, 0, len(dev.Channels))
	for _, ch := range dev.Channels {
		channels = append(channels, channelDTO{
			ChannelID:    ch.ChannelID,
			Name:         ch.Name,
			Manufacturer: ch.Manufacturer,
			Status:       ch.Status,
			PTZType:      ch.PTZType,
			Latitude:     ch.Latitude,
			Longitude:    ch.Longitude,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"device_id":      dev.DeviceID,
		"remote_addr":    dev.RemoteAddr,
		"transport":      dev.Transport,
		"status":         dev.Status.String(),
		"registered_at":  dev.RegisteredAt,
		"last_keepalive": dev.LastKeepalive,
		"channels":       channels,
	})
}

func (m *Module) apiDeleteDevice(w http.ResponseWriter, r *http.Request) {
	deviceID := extractPathParam(r.URL.Path, apiPrefix+"/devices/")
	if deviceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing device_id"})
		return
	}

	if m.registry.Get(deviceID) == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}

	m.sessions.CloseByDevice(deviceID)
	m.registry.Unregister(deviceID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "unregistered"})
}

// --- Channel endpoints ---

func (m *Module) apiListChannels(w http.ResponseWriter, r *http.Request) {
	deviceID := r.URL.Query().Get("device_id")
	type channelDTO struct {
		ChannelID    string  `json:"channel_id"`
		Name         string  `json:"name"`
		Manufacturer string  `json:"manufacturer"`
		Status       string  `json:"status"`
		PTZType      int     `json:"ptz_type"`
		Latitude     float64 `json:"latitude"`
		Longitude    float64 `json:"longitude"`
	}

	var channels []*Channel
	if deviceID != "" {
		dev := m.registry.Get(deviceID)
		if dev == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
			return
		}
		for _, ch := range dev.Channels {
			channels = append(channels, ch)
		}
	} else {
		channels = m.registry.AllChannels()
	}

	out := make([]channelDTO, 0, len(channels))
	for _, ch := range channels {
		out = append(out, channelDTO{
			ChannelID:    ch.ChannelID,
			Name:         ch.Name,
			Manufacturer: ch.Manufacturer,
			Status:       ch.Status,
			PTZType:      ch.PTZType,
			Latitude:     ch.Latitude,
			Longitude:    ch.Longitude,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// apiChannelAction handles POST/DELETE on /channels/{channelID}/{action}
// Routes:
//
//	POST   /channels/{channelID}/play      - start live view
//	DELETE /channels/{channelID}/play      - stop live view
//	POST   /channels/{channelID}/playback  - start playback
//	DELETE /channels/{channelID}/playback  - stop playback
//	POST   /channels/{channelID}/ptz       - PTZ control
//	POST   /channels/{channelID}/catalog   - catalog query
func (m *Module) apiChannelAction(w http.ResponseWriter, r *http.Request) {
	rest := extractPathParam(r.URL.Path, apiPrefix+"/channels/")
	// rest = "{channelID}/{action}"
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid path, expected /channels/{channelID}/{action}"})
		return
	}
	channelID := parts[0]
	action := parts[1]

	switch {
	case action == "play" && r.Method == http.MethodPost:
		m.apiPlay(w, r, channelID)
	case action == "play" && r.Method == http.MethodDelete:
		m.apiStopPlay(w, r, channelID)
	case action == "playback" && r.Method == http.MethodPost:
		m.apiPlayback(w, r, channelID)
	case action == "playback" && r.Method == http.MethodDelete:
		m.apiStopPlayback(w, r, channelID)
	case action == "ptz" && r.Method == http.MethodPost:
		m.apiPTZ(w, r, channelID)
	case action == "catalog" && r.Method == http.MethodPost:
		m.apiCatalog(w, r, channelID)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("unknown action: %s %s", r.Method, action)})
	}
}

func (m *Module) apiPlay(w http.ResponseWriter, r *http.Request, channelID string) {
	device, _ := m.registry.FindChannel(channelID)
	if device == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}

	session, err := m.invite.invite(r.Context(), device, channelID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"stream_key": session.StreamKey,
		"session_id": session.ID,
	})
}

func (m *Module) apiStopPlay(w http.ResponseWriter, r *http.Request, channelID string) {
	sessions := m.sessions.GetByChannel(channelID)
	if len(sessions) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no active session for channel"})
		return
	}

	for _, session := range sessions {
		session.Close()
		m.sessions.Remove(session.ID)
		if session.Stream != nil {
			session.Stream.RemovePublisher()
		}
		m.handler.ports.Free(session.LocalPort, session.LocalPort+1)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (m *Module) apiPlayback(w http.ResponseWriter, r *http.Request, channelID string) {
	var req struct {
		StartTime string `json:"start_time"`
		EndTime   string `json:"end_time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.StartTime == "" || req.EndTime == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "start_time, end_time required"})
		return
	}

	startTime, err := time.Parse(time.RFC3339, req.StartTime)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid start_time format (use RFC3339)"})
		return
	}
	endTime, err := time.Parse(time.RFC3339, req.EndTime)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid end_time format (use RFC3339)"})
		return
	}

	device, _ := m.registry.FindChannel(channelID)
	if device == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}

	session, err := m.playback.playback(r.Context(), device, channelID, startTime, endTime)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"stream_key": session.StreamKey,
		"session_id": session.ID,
	})
}

func (m *Module) apiStopPlayback(w http.ResponseWriter, r *http.Request, channelID string) {
	// Same logic as stop play — close sessions for this channel
	m.apiStopPlay(w, r, channelID)
}

func (m *Module) apiPTZ(w http.ResponseWriter, r *http.Request, channelID string) {
	var req struct {
		Command string `json:"command"`
		HSpeed  uint8  `json:"h_speed"`
		VSpeed  uint8  `json:"v_speed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Command == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "command required"})
		return
	}

	device, _ := m.registry.FindChannel(channelID)
	if device == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}

	var ptzCmd string
	switch strings.ToLower(req.Command) {
	case "up":
		ptzCmd = BuildPTZCmd(PTZUp, PTZZoomStop, req.HSpeed, req.VSpeed)
	case "down":
		ptzCmd = BuildPTZCmd(PTZDown, PTZZoomStop, req.HSpeed, req.VSpeed)
	case "left":
		ptzCmd = BuildPTZCmd(PTZLeft, PTZZoomStop, req.HSpeed, req.VSpeed)
	case "right":
		ptzCmd = BuildPTZCmd(PTZRight, PTZZoomStop, req.HSpeed, req.VSpeed)
	case "upleft":
		ptzCmd = BuildPTZCmd(PTZUpLeft, PTZZoomStop, req.HSpeed, req.VSpeed)
	case "upright":
		ptzCmd = BuildPTZCmd(PTZUpRight, PTZZoomStop, req.HSpeed, req.VSpeed)
	case "downleft":
		ptzCmd = BuildPTZCmd(PTZDownLeft, PTZZoomStop, req.HSpeed, req.VSpeed)
	case "downright":
		ptzCmd = BuildPTZCmd(PTZDownRight, PTZZoomStop, req.HSpeed, req.VSpeed)
	case "zoomin":
		ptzCmd = BuildPTZCmd(PTZStop, PTZZoomIn, 0, 0)
	case "zoomout":
		ptzCmd = BuildPTZCmd(PTZStop, PTZZoomOut, 0, 0)
	case "stop":
		ptzCmd = BuildPTZStopCmd()
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("unknown command: %s", req.Command)})
		return
	}

	body := BuildPTZControl(1, channelID, ptzCmd)
	if err := sendSIPMessage(r.Context(), m.sipService, device, channelID, body); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (m *Module) apiCatalog(w http.ResponseWriter, r *http.Request, channelID string) {
	// channelID here is actually used as deviceID for catalog query context
	// but we look up the parent device from the channel
	device, _ := m.registry.FindChannel(channelID)
	if device == nil {
		// Try as device ID directly
		device = m.registry.Get(channelID)
	}
	if device == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}

	if err := m.catalog.query(r.Context(), device); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "query sent"})
}

// --- Session endpoints ---

func (m *Module) apiListSessions(w http.ResponseWriter, r *http.Request) {
	sessions := m.sessions.All()
	type sessionDTO struct {
		ID        string `json:"id"`
		DeviceID  string `json:"device_id"`
		ChannelID string `json:"channel_id"`
		StreamKey string `json:"stream_key"`
		Direction string `json:"direction"`
		State     string `json:"state"`
		LocalPort int    `json:"local_port"`
		Transport string `json:"transport"`
	}
	out := make([]sessionDTO, 0, len(sessions))
	for _, s := range sessions {
		dir := "inbound"
		if s.Direction == SessionDirectionOutbound {
			dir = "outbound"
		}
		out = append(out, sessionDTO{
			ID:        s.ID,
			DeviceID:  s.DeviceID,
			ChannelID: s.ChannelID,
			StreamKey: s.StreamKey,
			Direction: dir,
			State:     s.GetState().String(),
			LocalPort: s.LocalPort,
			Transport: s.Transport,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (m *Module) apiDeleteSession(w http.ResponseWriter, r *http.Request) {
	sessionID := extractPathParam(r.URL.Path, apiPrefix+"/sessions/")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing session_id"})
		return
	}

	session := m.sessions.Get(sessionID)
	if session == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}

	session.Close()
	m.sessions.Remove(sessionID)
	if session.Stream != nil {
		session.Stream.RemovePublisher()
	}
	m.handler.ports.Free(session.LocalPort, session.LocalPort+1)

	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// --- Helpers ---

// sendSIPMessage sends a SIP MESSAGE with MANSCDP+xml body to a device.
func sendSIPMessage(ctx context.Context, sipSvc sipmod.SIPService, device *Device, targetID string, body []byte) error {
	remoteIP := extractIP(device.RemoteAddr)
	remotePort := parsePort(device.RemoteAddr)

	toURI := sip.Uri{
		User: targetID,
		Host: remoteIP,
		Port: remotePort,
	}

	req := sip.NewRequest(sip.MESSAGE, toURI)
	req.AppendHeader(sip.NewHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%s", sipSvc.ServerID(), sipSvc.Domain(), generateTag())))
	req.AppendHeader(sip.NewHeader("To", fmt.Sprintf("<sip:%s@%s>", targetID, remoteIP)))
	req.AppendHeader(sip.NewHeader("Content-Type", "Application/MANSCDP+xml"))
	req.SetBody(body)

	_, err := sipSvc.SendRequest(ctx, req)
	return err
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data) //nolint:errcheck
}
