package sipgateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/avframe"
	mediarp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/sdp"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
)

const sipLabTerminalHistoryLimit = 16

type labManager struct {
	mu         sync.RWMutex
	gateway    *Gateway
	sessions   map[string]*sipLabSession
	identities map[string]string
}

func newLabManager(gateway *Gateway) *labManager {
	return &labManager{
		gateway:    gateway,
		sessions:   make(map[string]*sipLabSession),
		identities: make(map[string]string),
	}
}

func (m *labManager) Start(ctx context.Context, request LabSessionRequest) (LabSessionSnapshot, error) {
	return m.start(ctx, request)
}

func (m *labManager) List() []LabSessionSnapshot { return m.list() }

func (m *labManager) Stop(id string) error { return m.stop(id) }

func (m *labManager) start(ctx context.Context, request LabSessionRequest) (LabSessionSnapshot, error) {
	if err := validateLabRequest(request); err != nil {
		return LabSessionSnapshot{}, err
	}
	identity := request.DeviceID
	m.mu.Lock()
	if existingID, ok := m.identities[identity]; ok {
		existing := m.sessions[existingID]
		if existing != nil && existing.isReserved() {
			m.mu.Unlock()
			return LabSessionSnapshot{}, ErrLabDuplicateIdentity
		}
		delete(m.identities, identity)
	}
	id := uuid.NewString()
	session := newSIPLabSession(id, identity, request, m.gateway)
	m.sessions[id] = session
	m.identities[identity] = id
	m.mu.Unlock()

	if err := session.start(ctx); err != nil {
		session.fail(err)
		m.mu.Lock()
		if m.identities[identity] == id {
			delete(m.identities, identity)
		}
		m.pruneTerminalsLocked()
		m.mu.Unlock()
		return session.snapshot(), err
	}
	return session.snapshot(), nil
}

func (m *labManager) list() []LabSessionSnapshot {
	m.mu.RLock()
	result := make([]LabSessionSnapshot, 0, len(m.sessions))
	for _, session := range m.sessions {
		result = append(result, session.snapshot())
	}
	m.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (m *labManager) stop(id string) error {
	m.mu.RLock()
	session := m.sessions[id]
	m.mu.RUnlock()
	if session == nil {
		return ErrLabSessionNotFound
	}
	if err := session.stop(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.identities[session.identity] == id {
		delete(m.identities, session.identity)
	}
	m.pruneTerminalsLocked()
	m.mu.Unlock()
	return nil
}

func (m *labManager) pruneTerminalsLocked() {
	type terminalSession struct {
		id        string
		updatedAt time.Time
	}
	terminal := make([]terminalSession, 0, len(m.sessions))
	for id, session := range m.sessions {
		if session == nil || session.isReserved() {
			continue
		}
		session.mu.RLock()
		updatedAt := session.updatedAt
		session.mu.RUnlock()
		terminal = append(terminal, terminalSession{id: id, updatedAt: updatedAt})
	}
	if len(terminal) <= sipLabTerminalHistoryLimit {
		return
	}
	sort.Slice(terminal, func(i, j int) bool {
		if terminal[i].updatedAt.Equal(terminal[j].updatedAt) {
			return terminal[i].id < terminal[j].id
		}
		return terminal[i].updatedAt.Before(terminal[j].updatedAt)
	})
	for _, session := range terminal[:len(terminal)-sipLabTerminalHistoryLimit] {
		delete(m.sessions, session.id)
	}
}

func (m *labManager) closeAll() {
	m.mu.RLock()
	sessions := make([]*sipLabSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.RUnlock()
	for _, session := range sessions {
		_ = session.stop()
	}
}

func validateLabRequest(request LabSessionRequest) error {
	if request.Mode != LabModePublish && request.Mode != LabModeReceive {
		return ErrLabInvalidRequest
	}
	if !validLabIdentity(request.DeviceID) || !validLabStreamKey(request.StreamKey) {
		return ErrLabInvalidRequest
	}
	switch strings.ToUpper(strings.TrimSpace(request.Codec)) {
	case "PCMA", "PCMU":
	default:
		return ErrLabInvalidRequest
	}
	return nil
}

func validLabIdentity(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("-_.", char) {
			continue
		}
		return false
	}
	return true
}

type sipLabSession struct {
	mu       sync.RWMutex
	id       string
	identity string
	request  LabSessionRequest
	gateway  *Gateway

	state     LabSessionState
	direction LabDirection
	startedAt time.Time
	updatedAt time.Time
	lastMedia time.Time
	stoppedAt time.Time
	callID    string
	invite    *sip.Request
	response  *sip.Response
	client    *sipgo.Client
	ua        *sipgo.UserAgent
	peer      *sipgo.Server

	ctx           context.Context
	cancel        context.CancelFunc
	peerCancel    context.CancelFunc
	peerDone      chan struct{}
	startDone     chan struct{}
	stopDone      chan struct{}
	stopOnce      sync.Once
	cleanupOnce   sync.Once
	stopRequested bool
	closeErr      error
	mediaWG       sync.WaitGroup
	rtpConn       *net.UDPConn
	rtcpConn      *net.UDPConn
	videoRTPConn  *net.UDPConn
	videoRTCPConn *net.UDPConn

	rtpPacketsSent  atomic.Uint64
	rtpPacketsRecv  atomic.Uint64
	rtpBytesSent    atomic.Uint64
	rtpBytesRecv    atomic.Uint64
	rtcpPacketsSent atomic.Uint64
	rtcpPacketsRecv atomic.Uint64
	audioRTPSent    atomic.Uint64
	audioRTPRecv    atomic.Uint64
	videoRTPSent    atomic.Uint64
	videoRTPRecv    atomic.Uint64
}

func newSIPLabSession(id, identity string, request LabSessionRequest, gateway *Gateway) *sipLabSession {
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	direction := LabDirectionInbound
	if request.Mode == LabModeReceive {
		direction = LabDirectionOutbound
	}
	return &sipLabSession{
		id:        id,
		identity:  identity,
		request:   request,
		gateway:   gateway,
		state:     LabSessionStateStarting,
		direction: direction,
		startedAt: now,
		updatedAt: now,
		ctx:       ctx,
		cancel:    cancel,
		startDone: make(chan struct{}),
		stopDone:  make(chan struct{}),
	}
}

func (s *sipLabSession) isReserved() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state == LabSessionStateStarting || s.state == LabSessionStateActive || s.state == LabSessionStateContract
}

func (s *sipLabSession) start(requestContext context.Context) error {
	defer close(s.startDone)
	if requestContext == nil {
		return ErrLabInvalidRequest
	}
	s.mu.RLock()
	state := s.state
	ctxErr := s.ctx.Err()
	s.mu.RUnlock()
	if state != LabSessionStateStarting {
		return ErrLabSessionNotFound
	}
	if ctxErr != nil {
		return ctxErr
	}
	protocolContext, release := s.protocolContext(requestContext)
	defer release()
	if s.gateway == nil {
		s.setState(LabSessionStateContract)
		return nil
	}
	switch s.request.Mode {
	case LabModePublish:
		return s.startPublish(protocolContext)
	case LabModeReceive:
		return s.startReceive(protocolContext)
	default:
		return ErrLabInvalidRequest
	}
}

func (s *sipLabSession) protocolContext(caller context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(caller)
	stopWatch := context.AfterFunc(s.ctx, cancel)
	return ctx, func() {
		stopWatch()
		cancel()
	}
}

func (s *sipLabSession) startPublish(requestContext context.Context) error {
	rtpConn, rtcpConn, err := listenLabUDPPair()
	if err != nil {
		return err
	}
	videoRTPConn, videoRTCPConn, err := listenLabUDPPair()
	if err != nil {
		_ = rtpConn.Close()
		_ = rtcpConn.Close()
		return err
	}
	s.mu.Lock()
	s.rtpConn, s.rtcpConn = rtpConn, rtcpConn
	s.videoRTPConn, s.videoRTCPConn = videoRTPConn, videoRTCPConn
	s.mu.Unlock()
	ua, client, err := newLabClient()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ua, s.client = ua, client
	s.mu.Unlock()

	host, port, err := gatewaySIPAddress(s.gateway.sipService.LocalAddr())
	if err != nil {
		return err
	}
	codec := strings.ToUpper(strings.TrimSpace(s.request.Codec))
	callID := uuid.NewString()
	invite := newLabInvite(sip.INVITE, sip.Uri{Scheme: "sip", User: s.request.DeviceID, Host: host, Port: port}, s.request.DeviceID, s.gateway.sipService.Domain(), callID)
	invite.SetBody(buildLabSDP(codec, rtpConn.LocalAddr().(*net.UDPAddr).Port, videoRTPConn.LocalAddr().(*net.UDPAddr).Port))
	invite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	invite.AppendHeader(sip.NewHeader(labStreamKeyHeader, s.request.StreamKey))
	invite.SetTransport("udp")
	response, err := sendLabInvite(requestContext, client, invite)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.callID, s.invite, s.response = callID, invite, response
	s.mu.Unlock()
	if err := sendLabACK(requestContext, client, invite, response); err != nil {
		return err
	}
	answer, err := sdp.Parse(response.Body())
	if err != nil {
		return fmt.Errorf("parse lab SIP answer: %w", err)
	}
	audio := firstAudioMedia(answer)
	if audio == nil || audio.Port <= 0 {
		return errors.New("lab SIP answer has no audio RTP port")
	}
	video := firstVideoMedia(answer)
	if video == nil || video.Port <= 0 {
		return errors.New("lab SIP answer has no video RTP port")
	}
	call, ok := s.gateway.Call(callID)
	if !ok || call.State != CallStateActive {
		return errors.New("lab publish call did not become active")
	}
	s.setState(LabSessionStateActive)
	s.mediaWG.Add(2)
	go s.publishAudioLoop(audio.Port, codec)
	go s.publishVideoLoop(video.Port)
	return nil
}

func (s *sipLabSession) startReceive(requestContext context.Context) error {
	stream, ok := s.gateway.hub.Find(s.request.StreamKey)
	if !ok || stream.Publisher() == nil {
		return fmt.Errorf("%w: receive stream %q", ErrStreamNotFound, s.request.StreamKey)
	}
	mediaInfo := stream.Publisher().MediaInfo()
	if mediaInfo == nil || codecNameForAV(mediaInfo.AudioCodec) != strings.ToUpper(strings.TrimSpace(s.request.Codec)) || mediaInfo.VideoCodec != avframe.CodecH264 {
		return ErrCodecMismatch
	}
	rtpConn, rtcpConn, err := listenLabUDPPair()
	if err != nil {
		return err
	}
	videoRTPConn, videoRTCPConn, err := listenLabUDPPair()
	if err != nil {
		_ = rtpConn.Close()
		_ = rtcpConn.Close()
		return err
	}
	s.mu.Lock()
	s.rtpConn, s.rtcpConn = rtpConn, rtcpConn
	s.videoRTPConn, s.videoRTCPConn = videoRTPConn, videoRTCPConn
	s.mu.Unlock()
	ua, err := sipgo.NewUA(sipgo.WithUserAgentHostname("lab.local"))
	if err != nil {
		return err
	}
	peer, err := sipgo.NewServer(ua)
	if err != nil {
		_ = ua.Close()
		return err
	}
	s.mu.Lock()
	s.ua, s.peer = ua, peer
	s.mu.Unlock()
	peerPort, err := freeLabPort()
	if err != nil {
		return err
	}
	peerAddr := net.JoinHostPort("127.0.0.1", fmt.Sprint(peerPort))
	peer.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		offer, parseErr := sdp.Parse(req.Body())
		if parseErr != nil {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil))
			return
		}
		audio := firstAudioMedia(offer)
		video := firstVideoMedia(offer)
		if audio == nil || video == nil {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil))
			return
		}
		codec := codecNameFromMedia(audio)
		if codec != "PCMA" && codec != "PCMU" {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil))
			return
		}
		if _, ok := negotiateH264(video); !ok {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil))
			return
		}
		response := sip.NewResponseFromRequest(req, 200, "OK", buildLabSDP(codec, rtpConn.LocalAddr().(*net.UDPAddr).Port, videoRTPConn.LocalAddr().(*net.UDPAddr).Port))
		response.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		if to := response.To(); to != nil {
			to.Params.Add("tag", "lab-"+s.id[:8])
		}
		s.mu.Lock()
		if s.invite == nil {
			s.invite = req
			s.response = response
		}
		s.mu.Unlock()
		_ = tx.Respond(response)
	})
	peer.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {})
	peer.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})
	peerCtx, peerCancel := context.WithCancel(s.ctx)
	s.mu.Lock()
	s.peerCancel, s.peerDone = peerCancel, make(chan struct{})
	peerDone := s.peerDone
	s.mu.Unlock()
	go func() {
		defer close(peerDone)
		_ = peer.ListenAndServe(peerCtx, "udp4", peerAddr)
	}()
	time.Sleep(15 * time.Millisecond)

	target := fmt.Sprintf("sip:%s@%s", s.request.DeviceID, peerAddr)
	callID, err := s.gateway.Dial(requestContext, target, s.request.StreamKey)
	if err != nil {
		return err
	}
	call, ok := s.gateway.Call(callID)
	if !ok || call.State != CallStateActive {
		return errors.New("lab receive call did not become active")
	}
	s.mu.Lock()
	s.callID = callID
	s.mu.Unlock()
	s.setState(LabSessionStateActive)
	s.mediaWG.Add(6)
	go s.receiveRTPLoop(rtpConn, false)
	go s.receiveRTPLoop(videoRTPConn, true)
	go s.receiveRTCPLoop(rtcpConn)
	go s.receiveRTCPLoop(videoRTCPConn)
	go s.sendReceiverReportLoop(rtcpConn, call.RTCPPort, 0x4c465241)
	go s.sendReceiverReportLoop(videoRTCPConn, call.VideoRTCPPort, 0x4c465256)
	return nil
}

func (s *sipLabSession) publishAudioLoop(remotePort int, codec string) {
	defer s.mediaWG.Done()
	remoteRTP := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: remotePort}
	remoteRTCP := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: remotePort + 1}
	seq := uint16(1)
	var dts int64
	ticker := time.NewTicker(time.Duration(labmedia.AudioFrameDurationMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		frame := labmedia.G711Frame(codecAV(codec), dts)
		packet := &pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: labPayloadType(codec), SequenceNumber: seq, Timestamp: uint32(dts * 8), SSRC: 0x4c465041}, Payload: frame.Payload}
		if data, err := packet.Marshal(); err == nil {
			if n, writeErr := s.rtpConn.WriteToUDP(data, remoteRTP); writeErr == nil {
				s.rtpPacketsSent.Add(1)
				s.audioRTPSent.Add(1)
				s.rtpBytesSent.Add(uint64(n))
				s.markMedia()
			}
		}
		if data, err := (&rtcp.ReceiverReport{SSRC: 0x4c465041}).Marshal(); err == nil {
			if _, writeErr := s.rtcpConn.WriteToUDP(data, remoteRTCP); writeErr == nil {
				s.rtcpPacketsSent.Add(1)
				s.markMedia()
			}
		}
		seq++
		dts += labmedia.AudioFrameDurationMs
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *sipLabSession) publishVideoLoop(remotePort int) {
	defer s.mediaWG.Done()
	remoteRTP := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: remotePort}
	remoteRTCP := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: remotePort + 1}
	session := mediarp.NewSession(96, 90000)
	packetizer := &mediarp.H264Packetizer{}
	var dts int64
	ticker := time.NewTicker(time.Duration(labmedia.VideoFrameDurationMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		packets, err := packetizer.Packetize(labmedia.VideoFrame(dts), 1200)
		if err == nil {
			session.WrapPackets(packets, dts)
			for _, packet := range packets {
				data, marshalErr := packet.Marshal()
				if marshalErr != nil {
					continue
				}
				if n, writeErr := s.videoRTPConn.WriteToUDP(data, remoteRTP); writeErr == nil {
					s.rtpPacketsSent.Add(1)
					s.videoRTPSent.Add(1)
					s.rtpBytesSent.Add(uint64(n))
					s.markMedia()
				}
			}
		}
		if data, reportErr := (&rtcp.ReceiverReport{SSRC: session.SSRC}).Marshal(); reportErr == nil {
			if _, writeErr := s.videoRTCPConn.WriteToUDP(data, remoteRTCP); writeErr == nil {
				s.rtcpPacketsSent.Add(1)
				s.markMedia()
			}
		}
		dts += labmedia.VideoFrameDurationMs
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *sipLabSession) receiveRTPLoop(conn *net.UDPConn, video bool) {
	defer s.mediaWG.Done()
	buf := make([]byte, 2048)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}
		var packet pionrtp.Packet
		if packet.Unmarshal(buf[:n]) == nil {
			s.rtpPacketsRecv.Add(1)
			if video {
				s.videoRTPRecv.Add(1)
			} else {
				s.audioRTPRecv.Add(1)
			}
			s.rtpBytesRecv.Add(uint64(n))
			s.markMedia()
		}
	}
}

func (s *sipLabSession) receiveRTCPLoop(conn *net.UDPConn) {
	defer s.mediaWG.Done()
	buf := make([]byte, 2048)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}
		packets, unmarshalErr := rtcp.Unmarshal(buf[:n])
		if unmarshalErr == nil {
			s.rtcpPacketsRecv.Add(uint64(len(packets)))
			s.markMedia()
		}
	}
}

func (s *sipLabSession) sendReceiverReportLoop(conn *net.UDPConn, remotePort int, ssrc uint32) {
	defer s.mediaWG.Done()
	if conn == nil || remotePort <= 0 {
		return
	}
	interval := time.Second
	if timeout := s.gateway.rtpIdleTimeout; timeout > 0 && timeout/3 < interval {
		interval = timeout / 3
	}
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	remote := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: remotePort}
	send := func() {
		data, err := (&rtcp.ReceiverReport{SSRC: ssrc}).Marshal()
		if err != nil {
			return
		}
		if _, err := conn.WriteToUDP(data, remote); err == nil {
			s.rtcpPacketsSent.Add(1)
			s.markMedia()
		}
	}
	send()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

func (s *sipLabSession) stop() error {
	s.stopOnce.Do(func() {
		defer close(s.stopDone)
		s.mu.Lock()
		s.stopRequested = true
		cancel := s.cancel
		s.mu.Unlock()
		cancel()
		<-s.startDone
		s.cleanup()
		s.mu.Lock()
		s.state = LabSessionStateStopped
		s.stoppedAt = time.Now().UTC()
		s.updatedAt = s.stoppedAt
		s.mu.Unlock()
	})
	<-s.stopDone
	s.mu.RLock()
	err := s.closeErr
	s.mu.RUnlock()
	return err
}

func (s *sipLabSession) cleanup() {
	s.cleanupOnce.Do(func() {
		s.mu.RLock()
		callID, mode := s.callID, s.request.Mode
		invite, response, client := s.invite, s.response, s.client
		peerCancel, peerDone, ua := s.peerCancel, s.peerDone, s.ua
		rtpConn, rtcpConn := s.rtpConn, s.rtcpConn
		videoRTPConn, videoRTCPConn := s.videoRTPConn, s.videoRTCPConn
		s.mu.RUnlock()
		if rtpConn != nil {
			_ = rtpConn.Close()
		}
		if rtcpConn != nil {
			_ = rtcpConn.Close()
		}
		if videoRTPConn != nil {
			_ = videoRTPConn.Close()
		}
		if videoRTCPConn != nil {
			_ = videoRTCPConn.Close()
		}
		var closeErr error
		if mode == LabModePublish && client != nil && invite != nil && response != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			closeErr = sendLabBYE(ctx, client, invite, response)
			cancel()
		}
		if s.gateway != nil && callID != "" {
			if err := s.gateway.Hangup(callID); err != nil && !errors.Is(err, ErrCallNotFound) {
				closeErr = errors.Join(closeErr, err)
			}
		}
		if peerCancel != nil {
			peerCancel()
		}
		if ua != nil {
			_ = ua.Close()
		}
		if peerDone != nil {
			<-peerDone
		}
		s.mediaWG.Wait()
		if closeErr != nil {
			s.mu.Lock()
			s.closeErr = closeErr
			s.mu.Unlock()
		}
	})
}

func (s *sipLabSession) fail(err error) {
	s.cancel()
	<-s.startDone
	s.cleanup()
	s.mu.Lock()
	if s.stopRequested || s.state == LabSessionStateStopped {
		s.mu.Unlock()
		return
	}
	s.state = LabSessionStateFailed
	s.updatedAt = time.Now().UTC()
	s.closeErr = err
	s.mu.Unlock()
}

func (s *sipLabSession) setState(state LabSessionState) {
	s.mu.Lock()
	s.state = state
	s.updatedAt = time.Now().UTC()
	s.mu.Unlock()
}

func (s *sipLabSession) markMedia() {
	s.mu.Lock()
	now := time.Now().UTC()
	s.lastMedia = now
	s.updatedAt = now
	s.mu.Unlock()
}

func (s *sipLabSession) snapshot() LabSessionSnapshot {
	s.mu.RLock()
	callID := s.callID
	mode := s.request.Mode
	result := LabSessionSnapshot{
		ID:          s.id,
		Identity:    s.identity,
		DeviceID:    s.request.DeviceID,
		StreamKey:   s.request.StreamKey,
		Mode:        s.request.Mode,
		State:       s.state,
		Direction:   s.direction,
		Codec:       strings.ToUpper(strings.TrimSpace(s.request.Codec)),
		LastError:   redactedTerminalError(s.closeErr),
		StartedAt:   s.startedAt,
		UpdatedAt:   s.updatedAt,
		LastMediaAt: s.lastMedia,
		StoppedAt:   s.stoppedAt,
	}
	s.mu.RUnlock()
	result.RTPPacketsSent = s.rtpPacketsSent.Load()
	result.RTPPacketsRecv = s.rtpPacketsRecv.Load()
	result.AudioRTPPacketsSent = s.audioRTPSent.Load()
	result.AudioRTPPacketsRecv = s.audioRTPRecv.Load()
	result.VideoRTPPacketsSent = s.videoRTPSent.Load()
	result.VideoRTPPacketsRecv = s.videoRTPRecv.Load()
	result.RTPBytesSent = s.rtpBytesSent.Load()
	result.RTPBytesRecv = s.rtpBytesRecv.Load()
	result.RTCPPacketsSent = s.rtcpPacketsSent.Load()
	result.RTCPPacketsRecv = s.rtcpPacketsRecv.Load()
	if mode == LabModePublish && s.gateway != nil && callID != "" {
		if call, ok := s.gateway.Call(callID); ok {
			result.RTCPPacketsSent = call.RTCPPacketsRecv
		}
	}
	return result
}

func newLabClient() (*sipgo.UserAgent, *sipgo.Client, error) {
	ua, err := sipgo.NewUA(sipgo.WithUserAgentHostname("lab.local"))
	if err != nil {
		return nil, nil, err
	}
	client, err := sipgo.NewClient(ua, sipgo.WithClientConnectionAddr("127.0.0.1:0"))
	if err != nil {
		_ = ua.Close()
		return nil, nil, err
	}
	return ua, client, nil
}

func newLabInvite(method sip.RequestMethod, recipient sip.Uri, user, domain, callID string) *sip.Request {
	request := sip.NewRequest(method, recipient)
	request.AppendHeader(&sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: user, Host: domain}, Params: sip.NewParams()})
	request.AppendHeader(&sip.ToHeader{Address: recipient})
	callIDHeader := sip.CallIDHeader(callID)
	request.AppendHeader(&callIDHeader)
	return request
}

func sendLabInvite(ctx context.Context, client *sipgo.Client, invite *sip.Request) (*sip.Response, error) {
	tx, err := client.TransactionRequest(ctx, invite)
	if err != nil {
		return nil, err
	}
	defer tx.Terminate()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case response, ok := <-tx.Responses():
			if !ok {
				return nil, errors.New("lab SIP INVITE transaction closed")
			}
			if response.StatusCode >= 200 {
				if response.StatusCode < 300 {
					return response, nil
				}
				return nil, fmt.Errorf("lab SIP INVITE rejected: %d %s", response.StatusCode, response.Reason)
			}
		}
	}
}

func sendLabACK(ctx context.Context, client *sipgo.Client, invite *sip.Request, response *sip.Response) error {
	ack := newLabDialogRequest(sip.ACK, invite, response, 1)
	if err := sipgo.ClientRequestBuild(client, ack); err != nil {
		return err
	}
	conn, err := client.TransportLayer().ClientRequestConnection(ctx, ack)
	if err != nil {
		return err
	}
	defer conn.TryClose()
	return conn.WriteMsg(ack)
}

func sendLabBYE(ctx context.Context, client *sipgo.Client, invite *sip.Request, response *sip.Response) error {
	bye := newLabDialogRequest(sip.BYE, invite, response, 2)
	resp, err := sendLabRequest(ctx, client, bye)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("lab SIP BYE rejected: %d %s", resp.StatusCode, resp.Reason)
	}
	return nil
}

func sendLabRequest(ctx context.Context, client *sipgo.Client, request *sip.Request) (*sip.Response, error) {
	tx, err := client.TransactionRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	defer tx.Terminate()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case response, ok := <-tx.Responses():
			if !ok {
				return nil, errors.New("lab SIP transaction closed")
			}
			if response.StatusCode >= 200 {
				return response, nil
			}
		}
	}
}

func newLabDialogRequest(method sip.RequestMethod, invite *sip.Request, response *sip.Response, cseq uint32) *sip.Request {
	request := sip.NewRequest(method, invite.Recipient)
	if from := invite.From(); from != nil {
		request.AppendHeader(sip.HeaderClone(from))
	}
	if to := response.To(); to != nil {
		request.AppendHeader(sip.HeaderClone(to))
	}
	if callID := invite.CallID(); callID != nil {
		request.AppendHeader(sip.HeaderClone(callID))
	}
	request.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: method})
	request.SetTransport(invite.Transport())
	return request
}

func gatewaySIPAddress(address string) (string, int, error) {
	if !strings.Contains(address, ":") {
		return address, 5060, nil
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, err
	}
	var port int
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil || port <= 0 {
		return "", 0, fmt.Errorf("invalid SIP service port %q", portText)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return host, port, nil
}

func listenLabUDP() (*net.UDPConn, error) {
	return net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
}

func listenLabUDPPair() (*net.UDPConn, *net.UDPConn, error) {
	for attempt := 0; attempt < 20; attempt++ {
		rtpConn, err := listenLabUDP()
		if err != nil {
			return nil, nil, err
		}
		rtpPort := rtpConn.LocalAddr().(*net.UDPAddr).Port
		rtcpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: rtpPort + 1})
		if err == nil {
			return rtpConn, rtcpConn, nil
		}
		_ = rtpConn.Close()
	}
	return nil, nil, errors.New("could not allocate a local RTP/RTCP pair")
}

func freeLabPort() (int, error) {
	conn, err := listenLabUDP()
	if err != nil {
		return 0, err
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	return port, conn.Close()
}

func buildLabSDP(codec string, audioPort, videoPort int) []byte {
	pt := labPayloadType(codec)
	return []byte(fmt.Sprintf("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=LiveForge SIP lab\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio %d RTP/AVP %d\r\na=rtpmap:%d %s/8000\r\nm=video %d RTP/AVP 96\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 packetization-mode=1;profile-level-id=42c00b\r\n", audioPort, pt, pt, codec, videoPort))
}

func firstAudioMedia(description *sdp.SessionDescription) *sdp.MediaDescription {
	if description == nil {
		return nil
	}
	for _, media := range description.Media {
		if media.Type == "audio" {
			return media
		}
	}
	return nil
}

func firstVideoMedia(description *sdp.SessionDescription) *sdp.MediaDescription {
	if description == nil {
		return nil
	}
	for _, media := range description.Media {
		if media.Type == "video" {
			return media
		}
	}
	return nil
}

func codecNameFromMedia(media *sdp.MediaDescription) string {
	if media == nil {
		return ""
	}
	for _, format := range media.Formats {
		for _, attribute := range media.Attributes {
			if attribute.Key != "rtpmap" {
				continue
			}
			fields := strings.Fields(attribute.Value)
			if len(fields) >= 2 && fields[0] == fmt.Sprint(format) {
				return strings.ToUpper(strings.Split(fields[1], "/")[0])
			}
		}
		if format == 8 {
			return "PCMA"
		}
		if format == 0 {
			return "PCMU"
		}
	}
	return ""
}

func labPayloadType(codec string) uint8 {
	if strings.EqualFold(codec, "PCMA") {
		return 8
	}
	return 0
}

func codecAV(codec string) avframe.CodecType {
	if strings.EqualFold(codec, "PCMA") {
		return avframe.CodecG711A
	}
	return avframe.CodecG711U
}

func codecNameForAV(codec avframe.CodecType) string {
	switch codec {
	case avframe.CodecG711A:
		return "PCMA"
	case avframe.CodecG711U:
		return "PCMU"
	default:
		return ""
	}
}
