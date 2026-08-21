package sipgateway

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/emiago/sipgo/sip"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/portalloc"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

// Gateway manages SIP-to-stream call bridging.
type Gateway struct {
	sipService sipmod.SIPService
	server     *core.Server
	hub        *core.StreamHub
	eventBus   *core.EventBus
	portAlloc  *portalloc.PortAllocator
	prefix     string
	maxCalls   int
	codecs     []string
	localIP    string

	mu       sync.Mutex
	sessions map[string]*CallSession
}

// NewGateway creates and starts a SIP gateway.
func NewGateway(cfg config.SIPGatewayConfig, sipSvc sipmod.SIPService, hub *core.StreamHub, bus *core.EventBus, servers ...*core.Server) (*Gateway, error) {
	if len(cfg.RTPPortRange) != 2 {
		return nil, fmt.Errorf("sipgateway: rtp_port_range must have exactly 2 elements [min, max]")
	}

	pa, err := portalloc.New(cfg.RTPPortRange[0], cfg.RTPPortRange[1])
	if err != nil {
		return nil, fmt.Errorf("sipgateway: port allocator: %w", err)
	}

	prefix := cfg.StreamPrefix
	if prefix == "" {
		prefix = "sip"
	}

	maxCalls := cfg.MaxCalls
	if maxCalls <= 0 {
		maxCalls = 100
	}

	codecs := cfg.Codecs
	if len(codecs) == 0 {
		codecs = []string{"opus", "PCMA", "PCMU"}
	}

	localIP := localAddress(sipSvc.LocalAddr())
	var server *core.Server
	if len(servers) > 0 {
		server = servers[0]
	}

	gw := &Gateway{
		sipService: sipSvc,
		server:     server,
		hub:        hub,
		eventBus:   bus,
		portAlloc:  pa,
		prefix:     prefix,
		maxCalls:   maxCalls,
		codecs:     codecs,
		localIP:    localIP,
		sessions:   make(map[string]*CallSession),
	}

	sipSvc.OnInvite(gw.handleInvite)
	sipSvc.OnBye(gw.handleBye)

	slog.Info("sip gateway enabled", "module", "sipgateway",
		"prefix", prefix, "max_calls", maxCalls, "codecs", codecs)

	return gw, nil
}

func (gw *Gateway) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	callID := ""
	if header := req.CallID(); header != nil {
		callID = header.Value()
	}
	streamKey := gw.streamKeyFromRequest(req)
	if err := gw.authorizeInbound(core.AuthorizationPreSession, req, streamKey, callID); err != nil {
		tx.Respond(sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)) //nolint:errcheck
		return
	}

	gw.mu.Lock()
	if _, exists := gw.sessions[callID]; exists {
		gw.mu.Unlock()
		resp := sip.NewResponseFromRequest(req, 486, "Busy Here", nil)
		tx.Respond(resp)
		return
	}
	if len(gw.sessions) >= gw.maxCalls {
		gw.mu.Unlock()
		resp := sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil)
		tx.Respond(resp)
		return
	}
	gw.mu.Unlock()

	body := req.Body()
	if len(body) == 0 {
		resp := sip.NewResponseFromRequest(req, 400, "Bad Request", nil)
		tx.Respond(resp)
		return
	}
	// GB28181 owns video INVITEs. SIP dispatch invokes all registered
	// handlers, so the audio gateway must leave video requests untouched for
	// the GB handler instead of sending a competing 488 response.
	if strings.Contains(strings.ToLower(string(body)), "m=video") {
		return
	}

	offerSDP, err := sdp.Parse(body)
	if err != nil {
		slog.Warn("invalid SDP in INVITE", "module", "sipgateway", "call", callID, "error", err)
		resp := sip.NewResponseFromRequest(req, 400, "Bad Request", nil)
		tx.Respond(resp)
		return
	}

	var audioMedia *sdp.MediaDescription
	for _, m := range offerSDP.Media {
		if m.Type == "audio" {
			audioMedia = m
			break
		}
	}
	if audioMedia == nil {
		resp := sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil)
		tx.Respond(resp)
		return
	}

	nc, ok := negotiateCodec(audioMedia, gw.codecs)
	if !ok {
		resp := sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil)
		tx.Respond(resp)
		return
	}
	if err := gw.authorizeInbound(core.AuthorizationPostConnect, req, streamKey, callID); err != nil {
		tx.Respond(sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)) //nolint:errcheck
		return
	}

	rtpPort, rtcpPort, err := gw.portAlloc.AllocatePair()
	if err != nil {
		slog.Error("port allocation failed", "module", "sipgateway", "error", err)
		resp := sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil)
		tx.Respond(resp)
		return
	}

	stream, streamExisted := gw.hub.Find(streamKey)
	if !streamExisted {
		stream, err = gw.hub.GetOrCreate(streamKey)
	}
	if err != nil {
		gw.portAlloc.Free(rtpPort, rtcpPort)
		resp := sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil)
		tx.Respond(resp)
		return
	}

	cs := newCallSession(callID, streamKey, nc, "inbound", rtpPort, rtcpPort)

	remoteIP := remoteAddress(offerSDP)
	if err := cs.startInbound(stream, remoteIP, audioMedia.Port); err != nil {
		gw.portAlloc.Free(rtpPort, rtcpPort)
		if !streamExisted {
			gw.hub.Destroy(streamKey, stream)
		}
		slog.Error("failed to start inbound session", "module", "sipgateway",
			"call", callID, "error", err)
		resp := sip.NewResponseFromRequest(req, 500, "Server Error", nil)
		tx.Respond(resp)
		return
	}

	gw.mu.Lock()
	gw.sessions[callID] = cs
	gw.mu.Unlock()

	answerBody := buildAnswerSDP(gw.localIP, rtpPort, nc)
	resp := sip.NewResponseFromRequest(req, 200, "OK", answerBody)
	resp.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	tx.Respond(resp)

	slog.Info("call established", "module", "sipgateway",
		"call", callID, "stream", streamKey, "codec", nc.EncodingName,
		"local_port", rtpPort, "remote", fmt.Sprintf("%s:%d", remoteIP, audioMedia.Port))
}

func (gw *Gateway) handleBye(req *sip.Request, tx sip.ServerTransaction) {
	callID := ""
	if header := req.CallID(); header != nil {
		callID = header.Value()
	}

	gw.mu.Lock()
	cs, ok := gw.sessions[callID]
	if ok {
		delete(gw.sessions, callID)
	}
	gw.mu.Unlock()

	if !ok {
		return
	}

	cs.Close()
	gw.portAlloc.Free(cs.rtpPort, cs.rtcpPort)

	if cs.stream != nil {
		cs.stream.RemovePublisherIfGeneration(cs.publisherGeneration)
	}

	resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
	tx.Respond(resp)

	slog.Info("call ended", "module", "sipgateway", "call", callID, "stream", cs.streamKey)
}

// Dial initiates an outbound call from a stream to a SIP URI.
func (gw *Gateway) Dial(ctx context.Context, targetURI, streamKey string) (string, error) {
	stream, ok := gw.hub.Find(streamKey)
	if !ok {
		return "", fmt.Errorf("stream %q not found", streamKey)
	}

	rtpPort, rtcpPort, err := gw.portAlloc.AllocatePair()
	if err != nil {
		return "", fmt.Errorf("port allocation: %w", err)
	}

	var offerCodecs []negotiatedCodec
	for _, name := range gw.codecs {
		if info, ok := encodingToCodec[name]; ok {
			offerCodecs = append(offerCodecs, negotiatedCodec{
				Codec:        info.Codec,
				PT:           info.PT,
				ClockRate:    info.ClockRate,
				EncodingName: name,
			})
		}
	}

	offerBody := buildOfferSDP(gw.localIP, rtpPort, offerCodecs)

	reqURI := sip.Uri{User: targetURI, Host: gw.sipService.Domain()}
	fromURI := sip.Uri{User: gw.sipService.ServerID(), Host: gw.sipService.Domain()}

	inviteReq := sip.NewRequest(sip.INVITE, reqURI)
	inviteReq.SetBody(offerBody)
	inviteReq.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	inviteReq.AppendHeader(&sip.FromHeader{Address: fromURI, Params: sip.NewParams()})
	inviteReq.AppendHeader(&sip.ToHeader{Address: reqURI})

	invTx, err := gw.sipService.SendInvite(ctx, inviteReq)
	if err != nil {
		gw.portAlloc.Free(rtpPort, rtcpPort)
		return "", fmt.Errorf("send INVITE: %w", err)
	}

	// Wait for final response
	select {
	case <-ctx.Done():
		gw.portAlloc.Free(rtpPort, rtcpPort)
		invTx.Close()
		return "", ctx.Err()
	case <-invTx.Done():
	}

	resp := invTx.Response()
	if resp == nil || resp.StatusCode != 200 {
		gw.portAlloc.Free(rtpPort, rtcpPort)
		if resp != nil {
			return "", fmt.Errorf("INVITE rejected: %d %s", resp.StatusCode, resp.Reason)
		}
		return "", fmt.Errorf("INVITE failed: no response")
	}

	answerSDP, err := sdp.Parse(resp.Body())
	if err != nil {
		gw.portAlloc.Free(rtpPort, rtcpPort)
		return "", fmt.Errorf("parse answer SDP: %w", err)
	}

	var audioMedia *sdp.MediaDescription
	for _, m := range answerSDP.Media {
		if m.Type == "audio" {
			audioMedia = m
			break
		}
	}
	if audioMedia == nil {
		gw.portAlloc.Free(rtpPort, rtcpPort)
		return "", fmt.Errorf("no audio in answer SDP")
	}

	nc, ok := negotiateCodec(audioMedia, gw.codecs)
	if !ok {
		gw.portAlloc.Free(rtpPort, rtcpPort)
		return "", fmt.Errorf("no common codec in answer")
	}

	if err := invTx.SendACK(ctx); err != nil {
		gw.portAlloc.Free(rtpPort, rtcpPort)
		return "", fmt.Errorf("send ACK: %w", err)
	}

	callID := inviteReq.CallID().Value()
	cs := newCallSession(callID, streamKey, nc, "outbound", rtpPort, rtcpPort)

	remoteIP := remoteAddress(answerSDP)
	if err := cs.startOutbound(stream, remoteIP, audioMedia.Port); err != nil {
		gw.portAlloc.Free(rtpPort, rtcpPort)
		return "", fmt.Errorf("start outbound: %w", err)
	}

	gw.mu.Lock()
	gw.sessions[callID] = cs
	gw.mu.Unlock()

	slog.Info("outbound call established", "module", "sipgateway",
		"call", callID, "target", targetURI, "stream", streamKey, "codec", nc.EncodingName)

	return callID, nil
}

// Hangup terminates a call by its call-ID.
func (gw *Gateway) Hangup(callID string) error {
	gw.mu.Lock()
	cs, ok := gw.sessions[callID]
	if ok {
		delete(gw.sessions, callID)
	}
	gw.mu.Unlock()

	if !ok {
		return fmt.Errorf("call %q not found", callID)
	}

	cs.Close()
	gw.portAlloc.Free(cs.rtpPort, cs.rtcpPort)

	if cs.direction == "inbound" && cs.stream != nil {
		cs.stream.RemovePublisherIfGeneration(cs.publisherGeneration)
	}

	return nil
}

// ActiveCalls returns the number of active calls.
func (gw *Gateway) ActiveCalls() int {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	return len(gw.sessions)
}

// Close stops all active calls and the gateway.
func (gw *Gateway) Close() {
	gw.mu.Lock()
	sessions := make(map[string]*CallSession, len(gw.sessions))
	for k, v := range gw.sessions {
		sessions[k] = v
	}
	gw.sessions = make(map[string]*CallSession)
	gw.mu.Unlock()

	for _, cs := range sessions {
		cs.Close()
		gw.portAlloc.Free(cs.rtpPort, cs.rtcpPort)
		if cs.direction == "inbound" && cs.stream != nil {
			cs.stream.RemovePublisherIfGeneration(cs.publisherGeneration)
		}
	}
}

func (gw *Gateway) authorizeInbound(stage core.AuthorizationStage, req *sip.Request, streamKey, callID string) error {
	if gw.server == nil {
		return nil
	}
	return gw.server.Authorize(context.Background(), core.AuthorizationRequest{
		Action:     core.AuthorizationPublish,
		Stage:      stage,
		StreamKey:  streamKey,
		Protocol:   "sipgateway",
		RemoteAddr: req.Source(),
		Params:     sipRequestParams(req),
		Extra: map[string]any{
			"sip_call_id": callID,
		},
	})
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

func (gw *Gateway) streamKeyFromRequest(req *sip.Request) string {
	user := req.Recipient.User
	if user == "" {
		user = req.CallID().Value()
	}
	return gw.prefix + "/" + user
}

func remoteAddress(sd *sdp.SessionDescription) string {
	if sd.Connection != nil {
		return sd.Connection.Address
	}
	for _, m := range sd.Media {
		if m.Connection != nil {
			return m.Connection.Address
		}
	}
	return ""
}

func localAddress(listenAddr string) string {
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "0.0.0.0"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		if ip := preferredOutboundIP(); ip != "" {
			return ip
		}
		return "0.0.0.0"
	}
	return host
}

func preferredOutboundIP() string {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr := conn.LocalAddr().(*net.UDPAddr)
	return addr.IP.String()
}
