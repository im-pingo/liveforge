package sipgateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/portalloc"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

var (
	errDuplicateCall = errors.New("duplicate call-ID")
)

type inviteDialog interface {
	Done() <-chan struct{}
	Response() *sip.Response
	SendACK(context.Context) error
	SendBYE(context.Context) error
	Close()
}

// Gateway manages SIP-to-stream call bridging.
type Gateway struct {
	sipService sipmod.SIPService
	hub        *core.StreamHub
	eventBus   *core.EventBus
	portAlloc  *portalloc.PortAllocator
	prefix     string
	maxCalls   int
	codecs     []string
	localIP    string
	sendInvite func(context.Context, *sip.Request) (inviteDialog, error)

	mu             sync.RWMutex
	sessions       map[string]*CallSession
	pending        map[string]struct{}
	closed         bool
	rtpIdleTimeout time.Duration
	metrics        gatewayMetrics
}

// NewGateway creates and starts a SIP gateway.
func NewGateway(cfg config.SIPGatewayConfig, sipSvc sipmod.SIPService, hub *core.StreamHub, bus *core.EventBus) (*Gateway, error) {
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

	gw := &Gateway{
		sipService:     sipSvc,
		hub:            hub,
		eventBus:       bus,
		portAlloc:      pa,
		prefix:         prefix,
		maxCalls:       maxCalls,
		codecs:         codecs,
		localIP:        localIP,
		sessions:       make(map[string]*CallSession),
		pending:        make(map[string]struct{}),
		rtpIdleTimeout: 30 * time.Second,
	}
	gw.sendInvite = func(ctx context.Context, req *sip.Request) (inviteDialog, error) {
		return sipSvc.SendInvite(ctx, req)
	}

	sipSvc.OnInvite(gw.handleInvite)
	sipSvc.OnBye(gw.handleBye)

	slog.Info("sip gateway enabled", "module", "sipgateway",
		"prefix", prefix, "max_calls", maxCalls, "codecs", codecs)

	return gw, nil
}

func (gw *Gateway) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	callIDHeader := req.CallID()
	if callIDHeader == nil || callIDHeader.Value() == "" {
		gw.metrics.setupFailures.Add(1)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", nil))
		return
	}
	callID := callIDHeader.Value()
	if err := gw.reserveCall(callID); err != nil {
		gw.metrics.setupFailures.Add(1)
		switch {
		case errors.Is(err, errDuplicateCall):
			gw.metrics.duplicateCallIDs.Add(1)
			_ = tx.Respond(sip.NewResponseFromRequest(req, 486, "Busy Here", nil))
		default:
			gw.metrics.capacityRejections.Add(1)
			_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
		}
		return
	}
	defer gw.cancelReservation(callID)

	body := req.Body()
	if len(body) == 0 {
		gw.metrics.setupFailures.Add(1)
		resp := sip.NewResponseFromRequest(req, 400, "Bad Request", nil)
		_ = tx.Respond(resp)
		return
	}

	offerSDP, err := sdp.Parse(body)
	if err != nil {
		gw.metrics.setupFailures.Add(1)
		slog.Warn("invalid SDP in INVITE", "module", "sipgateway", "call", callID, "error", err)
		resp := sip.NewResponseFromRequest(req, 400, "Bad Request", nil)
		_ = tx.Respond(resp)
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
		gw.metrics.setupFailures.Add(1)
		resp := sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil)
		_ = tx.Respond(resp)
		return
	}

	nc, ok := negotiateCodec(audioMedia, gw.codecs)
	if !ok {
		gw.metrics.setupFailures.Add(1)
		gw.metrics.codecFailures.Add(1)
		resp := sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil)
		_ = tx.Respond(resp)
		return
	}

	rtpPort, rtcpPort, err := gw.portAlloc.AllocatePair()
	if err != nil {
		gw.metrics.setupFailures.Add(1)
		gw.metrics.portExhaustions.Add(1)
		slog.Error("port allocation failed", "module", "sipgateway", "error", err)
		resp := sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil)
		_ = tx.Respond(resp)
		return
	}

	streamKey := gw.streamKeyFromRequest(req)
	stream, _ := gw.hub.GetOrCreate(streamKey)

	cs := newCallSession(callID, streamKey, nc, "inbound", rtpPort, rtcpPort)
	gw.configureSession(cs)
	if err := gw.activateReservedCall(cs); err != nil {
		gw.portAlloc.Free(rtpPort, rtcpPort)
		gw.metrics.setupFailures.Add(1)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
		return
	}

	remoteIP := remoteAddress(offerSDP)
	if err := cs.startInbound(stream, remoteIP, audioMedia.Port); err != nil {
		gw.metrics.setupFailures.Add(1)
		gw.finishSession(cs, CallStateEnded, err)
		slog.Error("failed to start inbound session", "module", "sipgateway",
			"call", callID, "error", err)
		resp := sip.NewResponseFromRequest(req, 500, "Server Error", nil)
		_ = tx.Respond(resp)
		return
	}
	gw.metrics.callsStarted.Add(1)

	answerBody := buildAnswerSDP(gw.localIP, rtpPort, nc)
	resp := sip.NewResponseFromRequest(req, 200, "OK", answerBody)
	resp.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	_ = tx.Respond(resp)

	slog.Info("call established", "module", "sipgateway",
		"call", callID, "stream", streamKey, "codec", nc.EncodingName,
		"local_port", rtpPort, "remote", fmt.Sprintf("%s:%d", remoteIP, audioMedia.Port))
}

func (gw *Gateway) handleBye(req *sip.Request, tx sip.ServerTransaction) {
	callIDHeader := req.CallID()
	if callIDHeader == nil || callIDHeader.Value() == "" {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", nil))
		return
	}
	callID := callIDHeader.Value()
	gw.mu.RLock()
	cs, ok := gw.sessions[callID]
	gw.mu.RUnlock()

	if !ok {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil))
		return
	}

	gw.finishSession(cs, CallStateEnded, nil)

	resp := sip.NewResponseFromRequest(req, 200, "OK", nil)
	_ = tx.Respond(resp)

	slog.Info("call ended", "module", "sipgateway", "call", callID, "stream", cs.streamKey)
}

// Dial initiates an outbound call from a stream to a SIP URI.
func (gw *Gateway) Dial(ctx context.Context, targetURI, streamKey string) (string, error) {
	stream, ok := gw.hub.Find(streamKey)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrStreamNotFound, streamKey)
	}
	if targetURI == "" {
		return "", ErrTargetRequired
	}

	callID := uuid.NewString()
	if err := gw.reserveCall(callID); err != nil {
		gw.metrics.setupFailures.Add(1)
		if errors.Is(err, ErrCallCapacity) {
			gw.metrics.capacityRejections.Add(1)
		}
		return "", err
	}
	defer gw.cancelReservation(callID)

	rtpPort, rtcpPort, err := gw.portAlloc.AllocatePair()
	if err != nil {
		gw.metrics.setupFailures.Add(1)
		gw.metrics.portExhaustions.Add(1)
		return "", fmt.Errorf("%w: %v", ErrPortExhausted, err)
	}
	portsOwned := true
	defer func() {
		if portsOwned {
			gw.portAlloc.Free(rtpPort, rtcpPort)
		}
	}()

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
	callIDHeader := sip.CallIDHeader(callID)
	inviteReq.AppendHeader(&callIDHeader)

	invTx, err := gw.sendInvite(ctx, inviteReq)
	if err != nil {
		gw.metrics.setupFailures.Add(1)
		return "", fmt.Errorf("send INVITE: %w", err)
	}

	// Wait for final response
	select {
	case <-ctx.Done():
		invTx.Close()
		gw.metrics.setupFailures.Add(1)
		return "", ctx.Err()
	case <-invTx.Done():
	}

	resp := invTx.Response()
	if resp == nil || resp.StatusCode != 200 {
		gw.metrics.setupFailures.Add(1)
		if resp != nil {
			return "", fmt.Errorf("INVITE rejected: %d %s", resp.StatusCode, resp.Reason)
		}
		return "", fmt.Errorf("INVITE failed: no response")
	}

	answerSDP, err := sdp.Parse(resp.Body())
	if err != nil {
		gw.metrics.setupFailures.Add(1)
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
		gw.metrics.setupFailures.Add(1)
		return "", fmt.Errorf("no audio in answer SDP")
	}

	nc, ok := negotiateCodec(audioMedia, gw.codecs)
	if !ok {
		gw.metrics.setupFailures.Add(1)
		gw.metrics.codecFailures.Add(1)
		return "", ErrCodecMismatch
	}

	if err := invTx.SendACK(ctx); err != nil {
		gw.metrics.setupFailures.Add(1)
		return "", fmt.Errorf("send ACK: %w", err)
	}

	cs := newCallSession(callID, streamKey, nc, "outbound", rtpPort, rtcpPort)
	cs.dialog = invTx
	gw.configureSession(cs)
	if err := gw.activateReservedCall(cs); err != nil {
		gw.metrics.setupFailures.Add(1)
		return "", err
	}
	portsOwned = false

	remoteIP := remoteAddress(answerSDP)
	if err := cs.startOutbound(stream, remoteIP, audioMedia.Port); err != nil {
		gw.metrics.setupFailures.Add(1)
		gw.finishSession(cs, CallStateEnded, err)
		return "", fmt.Errorf("start outbound: %w", err)
	}
	gw.metrics.callsStarted.Add(1)

	slog.Info("outbound call established", "module", "sipgateway",
		"call", callID, "target", targetURI, "stream", streamKey, "codec", nc.EncodingName)

	return callID, nil
}

// Hangup terminates a call by its call-ID.
func (gw *Gateway) Hangup(callID string) error {
	gw.mu.RLock()
	cs, ok := gw.sessions[callID]
	gw.mu.RUnlock()

	if !ok || !gw.finishSession(cs, CallStateEnded, nil) {
		return fmt.Errorf("%w: %q", ErrCallNotFound, callID)
	}

	var signalErr error
	if cs.dialog != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		signalErr = cs.dialog.SendBYE(ctx)
		cancel()
	}
	if signalErr != nil {
		return fmt.Errorf("send BYE: %w", signalErr)
	}
	return nil
}

// ActiveCalls returns the number of active calls.
func (gw *Gateway) ActiveCalls() int {
	gw.mu.RLock()
	defer gw.mu.RUnlock()
	return len(gw.sessions)
}

// ListCalls returns stable call snapshots sorted by call-ID.
func (gw *Gateway) ListCalls() []CallSnapshot {
	gw.mu.RLock()
	sessions := make([]*CallSession, 0, len(gw.sessions))
	for _, session := range gw.sessions {
		sessions = append(sessions, session)
	}
	gw.mu.RUnlock()

	calls := make([]CallSnapshot, 0, len(sessions))
	for _, session := range sessions {
		calls = append(calls, session.snapshot())
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].CallID < calls[j].CallID })
	return calls
}

// Call returns a stable snapshot for a call-ID.
func (gw *Gateway) Call(callID string) (CallSnapshot, bool) {
	gw.mu.RLock()
	session, ok := gw.sessions[callID]
	gw.mu.RUnlock()
	if !ok {
		return CallSnapshot{}, false
	}
	return session.snapshot(), true
}

// Metrics returns a stable snapshot without exposing per-call labels.
func (gw *Gateway) Metrics() MetricsSnapshot {
	snapshot := gw.metrics.snapshot()
	gw.mu.RLock()
	snapshot.ActiveCalls = len(gw.sessions)
	for _, session := range gw.sessions {
		switch session.direction {
		case "inbound":
			snapshot.ActiveInbound++
		case "outbound":
			snapshot.ActiveOutbound++
		}
	}
	gw.mu.RUnlock()
	return snapshot
}

// Close stops all active calls and the gateway.
func (gw *Gateway) Close() {
	gw.mu.Lock()
	if gw.closed {
		gw.mu.Unlock()
		return
	}
	gw.closed = true
	sessions := make([]*CallSession, 0, len(gw.sessions))
	for _, session := range gw.sessions {
		sessions = append(sessions, session)
	}
	gw.mu.Unlock()

	for _, session := range sessions {
		gw.finishSession(session, CallStateEnded, nil)
	}
}

func (gw *Gateway) reserveCall(callID string) error {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.closed {
		return ErrGatewayClosed
	}
	if _, exists := gw.sessions[callID]; exists {
		return errDuplicateCall
	}
	if _, exists := gw.pending[callID]; exists {
		return errDuplicateCall
	}
	if len(gw.sessions)+len(gw.pending) >= gw.maxCalls {
		return ErrCallCapacity
	}
	gw.pending[callID] = struct{}{}
	return nil
}

func (gw *Gateway) cancelReservation(callID string) {
	gw.mu.Lock()
	delete(gw.pending, callID)
	gw.mu.Unlock()
}

func (gw *Gateway) configureSession(session *CallSession) {
	session.metrics = &gw.metrics
	session.rtpIdleTimeout = gw.rtpIdleTimeout
	session.onTerminate = gw.sessionTerminated
}

func (gw *Gateway) activateReservedCall(session *CallSession) error {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if gw.closed {
		delete(gw.pending, session.callID)
		return ErrGatewayClosed
	}
	if _, reserved := gw.pending[session.callID]; !reserved {
		return fmt.Errorf("call %q has no setup reservation", session.callID)
	}
	delete(gw.pending, session.callID)
	gw.sessions[session.callID] = session
	return nil
}

func (gw *Gateway) sessionTerminated(session *CallSession, state CallState, err error) {
	gw.finishSession(session, state, err)
}

func (gw *Gateway) finishSession(session *CallSession, state CallState, err error) bool {
	gw.mu.Lock()
	current, ok := gw.sessions[session.callID]
	if !ok || current != session {
		gw.mu.Unlock()
		return false
	}
	delete(gw.sessions, session.callID)
	gw.mu.Unlock()

	session.terminate(state, err, false)
	gw.portAlloc.Free(session.rtpPort, session.rtcpPort)
	if session.direction == "inbound" && session.stream != nil && session.publisher != nil {
		publisher := session.stream.Publisher()
		if publisher != nil && publisher.ID() == session.publisher.id {
			session.stream.RemovePublisher()
		}
	}
	if session.established.Load() {
		gw.metrics.callsEnded.Add(1)
	}
	return true
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
