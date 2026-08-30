package gb28181

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/im-pingo/liveforge/internal/sipclose"
	sipmod "github.com/im-pingo/liveforge/module/sip"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
)

const (
	gbLabTerminalHistoryLimit = 16
	defaultGBLabMaxSessions   = 16
)

const (
	labRTPPayloadType = 96
	labSSRC           = 0x4c464742
	labMTU            = 1200
)

type labManager struct {
	mu          sync.RWMutex
	module      *Module
	sessions    map[string]*gbLabSession
	identities  map[string]string
	maxSessions int
}

func newLabManager(module *Module) *labManager {
	return newLabManagerWithLimit(module, defaultGBLabMaxSessions)
}

func newLabManagerWithLimit(module *Module, maxSessions int) *labManager {
	if maxSessions <= 0 {
		maxSessions = defaultGBLabMaxSessions
	}
	return &labManager{
		module:      module,
		sessions:    make(map[string]*gbLabSession),
		identities:  make(map[string]string),
		maxSessions: maxSessions,
	}
}

func (m *labManager) Start(ctx context.Context, request LabSessionRequest) (LabSessionSnapshot, error) {
	if err := validateGBLabRequest(request); err != nil {
		return LabSessionSnapshot{}, err
	}
	identity := request.DeviceID
	m.mu.Lock()
	if m.activeSessionsLocked() >= m.maxSessions {
		m.mu.Unlock()
		return LabSessionSnapshot{}, ErrLabCapacity
	}
	if existingID := m.identities[identity]; existingID != "" {
		if existing := m.sessions[existingID]; existing != nil && existing.isReserved() {
			m.mu.Unlock()
			return LabSessionSnapshot{}, ErrLabDuplicateIdentity
		}
		delete(m.identities, identity)
	}
	id := uuid.NewString()
	session := newGBLabSession(id, identity, request, m.module)
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
	go m.watchSession(session)
	return session.snapshot(), nil
}

func (m *labManager) activeSessionsLocked() int {
	active := 0
	for _, session := range m.sessions {
		if session != nil && session.isReserved() {
			active++
		}
	}
	return active
}

func (m *labManager) watchSession(session *gbLabSession) {
	receiver := session.inboundReceiver()
	sender := session.outboundSender()
	var receiverDone <-chan struct{}
	var senderDone <-chan error
	if receiver != nil {
		receiverDone = receiver.Done()
	}
	if sender != nil {
		senderDone = sender.done
	}
	var err error
	select {
	case <-session.ctx.Done():
		return
	case <-receiverDone:
		err = receiver.Err()
		if err == nil {
			err = errors.New("GB28181 server receiver ended")
		}
	case err = <-senderDone:
		if err == nil {
			err = errors.New("GB28181 outbound media ended")
		}
	case err = <-session.workerDone:
		if err == nil {
			err = errors.New("GB28181 Lab media worker ended")
		}
	}
	if session.ctx.Err() != nil {
		return
	}
	session.fail(err)
	m.mu.Lock()
	if m.identities[session.identity] == session.id {
		delete(m.identities, session.identity)
	}
	m.pruneTerminalsLocked()
	m.mu.Unlock()
}

func (m *labManager) List() []LabSessionSnapshot {
	m.mu.RLock()
	result := make([]LabSessionSnapshot, 0, len(m.sessions))
	for _, session := range m.sessions {
		result = append(result, session.snapshot())
	}
	m.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (m *labManager) Stop(id string) error {
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
	if len(terminal) <= gbLabTerminalHistoryLimit {
		return
	}
	sort.Slice(terminal, func(i, j int) bool {
		if terminal[i].updatedAt.Equal(terminal[j].updatedAt) {
			return terminal[i].id < terminal[j].id
		}
		return terminal[i].updatedAt.Before(terminal[j].updatedAt)
	})
	for _, session := range terminal[:len(terminal)-gbLabTerminalHistoryLimit] {
		delete(m.sessions, session.id)
	}
}

func (m *labManager) closeAll() {
	m.mu.RLock()
	sessions := make([]*gbLabSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.RUnlock()
	for _, session := range sessions {
		_ = session.stop()
	}
}

func validateGBLabRequest(request LabSessionRequest) error {
	if request.Mode != LabModePublish && request.Mode != LabModeReceive {
		return ErrLabInvalidRequest
	}
	if !validGBLabIdentity(request.DeviceID) || !validGBLabIdentity(request.ChannelID) || !validGBLabStreamKey(request.StreamKey) {
		return ErrLabInvalidRequest
	}
	return nil
}

func validGBLabStreamKey(value string) bool {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

func validGBLabIdentity(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("-_.,", char) {
			continue
		}
		return false
	}
	return true
}

type gbLabSession struct {
	mu       sync.RWMutex
	id       string
	identity string
	request  LabSessionRequest
	module   *Module

	state     LabSessionState
	direction LabDirection
	startedAt time.Time
	updatedAt time.Time
	lastMedia time.Time
	stoppedAt time.Time
	closeErr  error

	ctx    context.Context
	cancel context.CancelFunc

	startDone   chan struct{}
	stopDone    chan struct{}
	stopOnce    sync.Once
	cleanupOnce sync.Once
	stopRequest bool
	mediaWG     sync.WaitGroup
	workerDone  chan error
	controlWG   sync.WaitGroup

	client         *sipgo.Client
	clientUA       *sipgo.UserAgent
	sipConns       []sip.Connection
	unregisterFunc func(context.Context, *sipgo.Client, string, int, string) (*sip.Response, error)
	peerUA         *sipgo.UserAgent
	peer           *sipgo.Server
	peerConn       net.PacketConn
	peerCancel     context.CancelFunc
	peerDone       chan struct{}

	inviteRequest  *sip.Request
	inviteResponse *sip.Response
	inviteTx       *sipmod.InviteTransaction
	moduleSession  *MediaSession
	callID         string
	peerRemotePort int

	rtpConn            *net.UDPConn
	rtcpConn           *net.UDPConn
	rtpTimestampOffset uint32

	rtpPacketsSent  atomic.Uint64
	rtpPacketsRecv  atomic.Uint64
	rtpBytesSent    atomic.Uint64
	rtpBytesRecv    atomic.Uint64
	rtcpPacketsSent atomic.Uint64
	rtcpPacketsRecv atomic.Uint64
	psFramesSent    atomic.Uint64
	psFramesRecv    atomic.Uint64
	audioFramesSent atomic.Uint64
	audioFramesRecv atomic.Uint64
	videoFramesSent atomic.Uint64
	videoFramesRecv atomic.Uint64
	catalogSent     atomic.Bool
	inviteReceived  atomic.Bool
	ackReceived     atomic.Bool
	byeReceived     atomic.Bool
}

func newGBLabSession(id, identity string, request LabSessionRequest, module *Module) *gbLabSession {
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	direction := LabDirectionInbound
	if request.Mode == LabModeReceive {
		direction = LabDirectionOutbound
	}
	return &gbLabSession{
		id:                 id,
		identity:           identity,
		request:            request,
		module:             module,
		state:              LabSessionStateStarting,
		direction:          direction,
		startedAt:          now,
		updatedAt:          now,
		ctx:                ctx,
		cancel:             cancel,
		startDone:          make(chan struct{}),
		stopDone:           make(chan struct{}),
		workerDone:         make(chan error, 2),
		rtpTimestampOffset: randomGBLabTimestampOffset(),
	}
}

func randomGBLabTimestampOffset() uint32 {
	var value [4]byte
	if _, err := rand.Read(value[:]); err == nil {
		return binary.BigEndian.Uint32(value[:])
	}
	return uint32(time.Now().UnixNano())
}

func (s *gbLabSession) isReserved() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state == LabSessionStateStarting || s.state == LabSessionStateActive
}

func (s *gbLabSession) start(caller context.Context) error {
	defer close(s.startDone)
	if caller == nil {
		return ErrLabInvalidRequest
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	ctx, release := s.protocolContext(caller)
	defer release()
	if s.request.Mode == LabModePublish {
		return s.startPublish(ctx)
	}
	return s.startReceive(ctx)
}

func (s *gbLabSession) protocolContext(caller context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(caller)
	stopWatch := context.AfterFunc(s.ctx, cancel)
	return ctx, func() {
		stopWatch()
		cancel()
	}
}

func (s *gbLabSession) startPublish(ctx context.Context) error {
	if err := s.openMediaPair(); err != nil {
		return err
	}
	if err := s.openPeer(); err != nil {
		return err
	}
	if err := s.openClient(); err != nil {
		return err
	}
	serverHost, serverPort, err := sipAddress(s.module.sipService.LocalAddr())
	if err != nil {
		return err
	}
	if _, err := s.register(ctx, serverHost, serverPort); err != nil {
		return err
	}
	if err := s.sendKeepalive(ctx, serverHost, serverPort); err != nil {
		return err
	}
	if err := s.sendCatalog(ctx, serverHost, serverPort); err != nil {
		return err
	}

	device, channel := s.module.registry.FindChannel(s.request.ChannelID)
	if device == nil || channel == nil || device.DeviceID != s.request.DeviceID {
		return errors.New("GB28181 lab catalog channel was not registered")
	}
	moduleSession, err := s.module.invite.inviteStream(ctx, device, s.request.ChannelID, s.request.StreamKey, nil)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.moduleSession = moduleSession
	s.callID = moduleSession.Snapshot().ID
	remotePort := s.peerRemotePort
	s.mu.Unlock()
	ackDeadline := time.NewTimer(250 * time.Millisecond)
	defer ackDeadline.Stop()
	for s.inviteReceived.Load() && !s.ackReceived.Load() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ackDeadline.C:
			return errors.New("GB28181 lab fake device did not receive server ACK")
		case <-time.After(time.Millisecond):
		}
	}
	if !s.inviteReceived.Load() {
		return errors.New("GB28181 lab fake device did not complete server INVITE/ACK")
	}
	if remotePort <= 0 {
		return errors.New("GB28181 lab server INVITE has no RTP port")
	}
	s.setState(LabSessionStateActive)
	s.startKeepaliveLoop(serverHost, serverPort)
	s.startMediaWorker("publish media", func() error { return s.publishMediaLoop(remotePort) })
	return nil
}

func (s *gbLabSession) startReceive(ctx context.Context) error {
	source, err := s.module.prepareGBOutboundMedia(ctx, s.request.StreamKey)
	if err != nil {
		return err
	}
	sourceCtx, cancelSource := bindGBGeneration(ctx, source.snapshot)
	defer cancelSource()
	if err := s.openMediaPair(); err != nil {
		return err
	}
	if err := s.openPeer(); err != nil {
		return err
	}
	if err := s.openClient(); err != nil {
		return err
	}
	serverHost, serverPort, err := sipAddress(s.module.sipService.LocalAddr())
	if err != nil {
		return err
	}
	if !source.stream.IsPublisherGeneration(source.snapshot.Generation) {
		return errors.New("GB28181 outbound media source generation ended")
	}
	if _, err := s.register(sourceCtx, serverHost, serverPort); err != nil {
		return err
	}
	// REGISTER proves the real device path. The GB handler records the source
	// address, while a SIP endpoint may advertise a separate Contact address.
	s.module.registry.Register(s.request.DeviceID, s.peerConn.LocalAddr().String(), "udp")
	if err := s.sendKeepalive(sourceCtx, serverHost, serverPort); err != nil {
		return err
	}
	if err := s.sendCatalog(sourceCtx, serverHost, serverPort); err != nil {
		return err
	}
	device, channel := s.module.registry.FindChannel(s.request.ChannelID)
	if device == nil || channel == nil || device.DeviceID != s.request.DeviceID {
		return errors.New("GB28181 lab catalog channel was not registered")
	}
	moduleSession, err := s.module.startOutboundMediaFromSource(sourceCtx, device, s.request.ChannelID, s.request.StreamKey, source)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.callID = moduleSession.Snapshot().ID
	s.moduleSession = moduleSession
	s.mu.Unlock()
	s.setState(LabSessionStateActive)
	s.startKeepaliveLoop(serverHost, serverPort)
	s.startMediaWorker("receive media", s.receiveMediaLoop)
	s.startMediaWorker("receive RTCP", s.receiveRTCPLoop)
	return nil
}

func (s *gbLabSession) startMediaWorker(name string, worker func() error) {
	s.mediaWG.Add(1)
	go func() {
		defer s.mediaWG.Done()
		err := worker()
		if err != nil {
			err = fmt.Errorf("GB28181 Lab %s: %w", name, err)
		}
		select {
		case s.workerDone <- err:
		default:
		}
	}()
}

func (s *gbLabSession) openMediaPair() error {
	rtpConn, rtcpConn, err := listenGBLabUDPPair()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.rtpConn, s.rtcpConn = rtpConn, rtcpConn
	s.mu.Unlock()
	return nil
}

func (s *gbLabSession) openClient() error {
	ua, err := sipgo.NewUA(sipgo.WithUserAgentHostname("gb-lab.local"))
	if err != nil {
		return err
	}
	client, err := sipgo.NewClient(ua, sipgo.WithClientConnectionAddr("127.0.0.1:0"))
	if err != nil {
		_ = ua.Close()
		return err
	}
	s.mu.Lock()
	s.clientUA, s.client = ua, client
	s.mu.Unlock()
	return nil
}

func (s *gbLabSession) openPeer() error {
	ua, err := sipgo.NewUA(sipgo.WithUserAgentHostname("gb-lab.local"))
	if err != nil {
		return err
	}
	peer, err := sipgo.NewServer(ua)
	if err != nil {
		_ = ua.Close()
		return err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		_ = ua.Close()
		return err
	}
	peer.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		if req.From() == nil || req.From().Address.User == "" || req.To() == nil || req.To().Address.User != s.request.ChannelID || len(req.Body()) == 0 {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", nil))
			return
		}
		answerDirection := "recvonly"
		if s.request.Mode == LabModePublish {
			answerDirection = "sendonly"
		}
		response := sip.NewResponseFromRequest(req, 200, "OK", buildGBLabSDP(s.rtpConn.LocalAddr().(*net.UDPAddr).Port, answerDirection))
		response.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		if to := response.To(); to != nil {
			to.Params.Add("tag", "gb-lab-"+s.id[:8])
		}
		s.mu.Lock()
		s.peerRemotePort = parseSDPPort(string(req.Body()))
		if s.inviteRequest == nil {
			s.inviteRequest, s.inviteResponse = req, response
		}
		s.mu.Unlock()
		s.inviteReceived.Store(true)
		_ = tx.Respond(response)
	})
	peer.OnAck(func(_ *sip.Request, _ sip.ServerTransaction) { s.ackReceived.Store(true) })
	peer.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		s.byeReceived.Store(true)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})
	peer.OnMessage(func(req *sip.Request, tx sip.ServerTransaction) {
		if ParseMessageType(req.Body()) == "Catalog" {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
			return
		}
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})
	_, peerCancel := context.WithCancel(s.ctx)
	peerDone := make(chan struct{})
	s.mu.Lock()
	s.peerUA, s.peer, s.peerConn, s.peerCancel, s.peerDone = ua, peer, conn, peerCancel, peerDone
	s.mu.Unlock()
	go func() {
		defer close(peerDone)
		_ = peer.ServeUDP(conn)
	}()
	return nil
}

func (s *gbLabSession) register(ctx context.Context, host string, port int) (*sip.Response, error) {
	req := newGBLabRequest(sip.REGISTER, sip.Uri{Scheme: "sip", User: s.request.DeviceID, Host: host, Port: port}, s.request.DeviceID, s.module.sipService.Domain(), uuid.NewString())
	req.AppendHeader(sip.NewHeader("Expires", "3600"))
	s.mu.RLock()
	if s.peerConn != nil {
		req.AppendHeader(sip.NewHeader("Contact", fmt.Sprintf("<sip:%s@%s>", s.request.DeviceID, s.peerConn.LocalAddr())))
	}
	s.mu.RUnlock()
	return sendGBLabRequest(ctx, s.client, req, s.rememberSIPConnection)
}

func (s *gbLabSession) sendKeepalive(ctx context.Context, host string, port int) error {
	body := KeepaliveMessage{CmdType: "Keepalive", SN: 1, DeviceID: s.request.DeviceID, Status: "OK"}
	data, err := xml.Marshal(body)
	if err != nil {
		return err
	}
	req := newGBLabRequest(sip.MESSAGE, sip.Uri{Scheme: "sip", User: s.module.sipService.ServerID(), Host: host, Port: port}, s.request.DeviceID, s.module.sipService.Domain(), uuid.NewString())
	req.AppendHeader(sip.NewHeader("Content-Type", "Application/MANSCDP+xml"))
	req.SetBody(append([]byte(xml.Header), data...))
	response, err := sendGBLabRequest(ctx, s.client, req, s.rememberSIPConnection)
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("GB28181 lab keepalive: %w", joinSIPResponseError(err, response))
	}
	return nil
}

func gbLabKeepaliveInterval(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	interval := timeout / 3
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	return interval
}

func (s *gbLabSession) startKeepaliveLoop(host string, port int) {
	interval := gbLabKeepaliveInterval(s.module.registry.keepaliveTimeout)
	s.controlWG.Add(1)
	go func() {
		defer s.controlWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(s.ctx, interval)
				err := s.sendKeepalive(ctx, host, port)
				cancel()
				if err != nil {
					if s.ctx.Err() != nil {
						return
					}
					slog.Warn("GB28181 lab keepalive failed", "module", "gb28181", "device", s.request.DeviceID, "error", err)
				}
			}
		}
	}()
}

func (s *gbLabSession) sendCatalog(ctx context.Context, host string, port int) error {
	body := CatalogResponse{CmdType: "Catalog", SN: 1, DeviceID: s.request.DeviceID, SumNum: 1, DeviceList: CatalogDeviceList{Num: 1, Items: []CatalogItem{{DeviceID: s.request.ChannelID, Name: "LiveForge GB Lab Camera", Manufacturer: "LiveForge", Status: "ON"}}}}
	data, err := xml.Marshal(body)
	if err != nil {
		return err
	}
	req := newGBLabRequest(sip.MESSAGE, sip.Uri{Scheme: "sip", User: s.module.sipService.ServerID(), Host: host, Port: port}, s.request.DeviceID, s.module.sipService.Domain(), uuid.NewString())
	req.AppendHeader(sip.NewHeader("Content-Type", "Application/MANSCDP+xml"))
	req.SetBody(append([]byte(xml.Header), data...))
	response, err := sendGBLabRequest(ctx, s.client, req, s.rememberSIPConnection)
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("GB28181 lab catalog: %w", joinSIPResponseError(err, response))
	}
	s.catalogSent.Store(true)
	return nil
}

func (s *gbLabSession) publishMediaLoop(remotePort int) error {
	remoteRTP := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: remotePort}
	remoteRTCP := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: remotePort + 1}
	muxer := ps.NewMuxer()
	ticker := time.NewTicker(time.Duration(labmedia.AudioFrameDurationMs) * time.Millisecond)
	defer ticker.Stop()
	var timestamp int64
	for {
		if timestamp%labmedia.VideoFrameDurationMs == 0 {
			if err := s.sendFrame(remoteRTP, remoteRTCP, muxer, deterministicGBLabFrame(timestamp)); err != nil {
				if s.ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
		if err := s.sendFrame(remoteRTP, remoteRTCP, muxer, labmedia.G711Frame(avframe.CodecG711A, timestamp)); err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			return err
		}
		timestamp += labmedia.AudioFrameDurationMs
		select {
		case <-s.ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func deterministicGBLabFrame(timestamp int64) *avframe.AVFrame {
	return labmedia.VideoFrame(timestamp)
}

func deterministicGBLabH264Payload() []byte {
	return labmedia.VideoFrame(0).Payload
}

func (s *gbLabSession) sendFrame(remoteRTP, remoteRTCP *net.UDPAddr, muxer *ps.Muxer, frame *avframe.AVFrame) error {
	data, err := muxer.Pack(frame)
	if err != nil {
		return err
	}
	seq := uint16(s.rtpPacketsSent.Load() + 1)
	timestamp := s.rtpTimestampOffset + uint32(frame.DTS*90)
	for offset := 0; offset < len(data); {
		end := offset + labMTU
		if end > len(data) {
			end = len(data)
		}
		packet := &pionrtp.Packet{Header: pionrtp.Header{Version: 2, PayloadType: labRTPPayloadType, SequenceNumber: seq, Timestamp: timestamp, SSRC: labSSRC, Marker: end == len(data)}, Payload: data[offset:end]}
		packetData, marshalErr := packet.Marshal()
		if marshalErr != nil {
			return marshalErr
		}
		n, writeErr := s.rtpConn.WriteToUDP(packetData, remoteRTP)
		if writeErr != nil {
			return writeErr
		}
		s.rtpPacketsSent.Add(1)
		s.rtpBytesSent.Add(uint64(n))
		seq++
		offset = end
	}
	if rtcpData, marshalErr := (&rtcp.ReceiverReport{SSRC: labSSRC}).Marshal(); marshalErr == nil {
		if _, writeErr := s.rtcpConn.WriteToUDP(rtcpData, remoteRTCP); writeErr == nil {
			s.rtcpPacketsSent.Add(1)
		}
	}
	s.psFramesSent.Add(1)
	if frame.MediaType.IsAudio() {
		s.audioFramesSent.Add(1)
	} else if frame.MediaType.IsVideo() {
		s.videoFramesSent.Add(1)
	}
	s.markMedia()
	return nil
}

func (s *gbLabSession) receiveMediaLoop() error {
	publisher := NewPublisher("gb28181-lab-receiver", func(frame *avframe.AVFrame) {
		if frame == nil {
			return
		}
		s.psFramesRecv.Add(1)
		if frame.MediaType.IsAudio() && frame.Codec == avframe.CodecG711A {
			s.audioFramesRecv.Add(1)
		} else if frame.MediaType.IsVideo() && frame.Codec == avframe.CodecH264 {
			s.videoFramesRecv.Add(1)
		}
		s.markMedia()
	})
	buf := make([]byte, 64<<10)
	for {
		_ = s.rtpConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, _, err := s.rtpConn.ReadFromUDP(buf)
		if err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return err
		}
		var packet pionrtp.Packet
		if packet.Unmarshal(buf[:n]) != nil || packet.PayloadType != labRTPPayloadType {
			continue
		}
		s.rtpPacketsRecv.Add(1)
		s.rtpBytesRecv.Add(uint64(n))
		publisher.FeedRTP(&packet)
	}
}

func (s *gbLabSession) receiveRTCPLoop() error {
	buf := make([]byte, 2048)
	for {
		_ = s.rtcpConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, _, err := s.rtcpConn.ReadFromUDP(buf)
		if err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return err
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err == nil {
			s.rtcpPacketsRecv.Add(uint64(len(packets)))
			s.markMedia()
		}
	}
}

func (s *gbLabSession) stop() error {
	s.stopOnce.Do(func() {
		defer close(s.stopDone)
		s.mu.Lock()
		s.stopRequest = true
		s.mu.Unlock()
		s.cancel()
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

func (s *gbLabSession) fail(err error) {
	s.cancel()
	<-s.startDone
	s.cleanup()
	s.mu.Lock()
	if !s.stopRequest {
		s.state = LabSessionStateFailed
		s.closeErr = err
		s.updatedAt = time.Now().UTC()
	}
	s.mu.Unlock()
}

func (s *gbLabSession) outboundSender() *outboundMediaSession {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.moduleSession == nil {
		return nil
	}
	return s.moduleSession.Snapshot().Sender
}

func (s *gbLabSession) inboundReceiver() *RTPReceiver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.moduleSession == nil {
		return nil
	}
	return s.moduleSession.Snapshot().Receiver
}

func (s *gbLabSession) cleanup() {
	s.cleanupOnce.Do(func() {
		s.mu.RLock()
		client, clientUA, peerUA := s.client, s.clientUA, s.peerUA
		inviteTx, moduleSession := s.inviteTx, s.moduleSession
		peerCancel, peerConn, peerDone := s.peerCancel, s.peerConn, s.peerDone
		rtpConn, rtcpConn := s.rtpConn, s.rtcpConn
		deviceID := s.request.DeviceID
		s.mu.RUnlock()

		// Keepalive requests use the session SIP client. Stop that loop before
		// sending teardown requests or closing the client.
		s.controlWG.Wait()

		var cleanupErr error
		if moduleSession != nil {
			s.module.handler.closeSession(moduleSession, "")
		}
		if inviteTx != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			cleanupErr = errors.Join(cleanupErr, inviteTx.SendBYE(ctx))
			cancel()
			inviteTx.Close()
		}
		if client != nil {
			if host, port, err := sipAddress(s.module.sipService.LocalAddr()); err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				unregister := s.unregister
				if s.unregisterFunc != nil {
					unregister = s.unregisterFunc
				}
				if response, err := unregister(ctx, client, host, port, deviceID); err != nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
					cleanupErr = errors.Join(cleanupErr, joinSIPResponseError(err, response))
				}
				cancel()
			}
		}
		if rtpConn != nil {
			_ = rtpConn.Close()
		}
		if rtcpConn != nil {
			_ = rtcpConn.Close()
		}
		if peerCancel != nil {
			peerCancel()
		}
		if peerConn != nil {
			_ = peerConn.Close()
		}
		if peerDone != nil {
			<-peerDone
		}
		if peerUA != nil {
			_ = peerUA.Close()
		}
		if clientUA != nil {
			s.mu.RLock()
			sipConns := append([]sip.Connection(nil), s.sipConns...)
			s.mu.RUnlock()
			cleanupErr = errors.Join(cleanupErr, sipclose.CloseUserAgent(clientUA, sipConns))
		}
		s.mediaWG.Wait()
		if s.module.registry.Get(deviceID) != nil {
			s.module.registry.Unregister(deviceID)
		}
		if cleanupErr != nil {
			s.mu.Lock()
			s.closeErr = cleanupErr
			s.mu.Unlock()
		}
	})
}

func (s *gbLabSession) rememberSIPConnection(connection sip.Connection) {
	if connection == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.sipConns {
		if existing == connection {
			return
		}
	}
	s.sipConns = append(s.sipConns, connection)
}

func (s *gbLabSession) unregister(ctx context.Context, client *sipgo.Client, host string, port int, deviceID string) (*sip.Response, error) {
	req := newGBLabRequest(sip.REGISTER, sip.Uri{Scheme: "sip", User: deviceID, Host: host, Port: port}, deviceID, s.module.sipService.Domain(), uuid.NewString())
	req.AppendHeader(sip.NewHeader("Expires", "0"))
	return sendGBLabRequest(ctx, client, req, s.rememberSIPConnection)
}

func (s *gbLabSession) setState(state LabSessionState) {
	s.mu.Lock()
	s.state = state
	s.updatedAt = time.Now().UTC()
	s.mu.Unlock()
}

func (s *gbLabSession) markMedia() {
	s.mu.Lock()
	now := time.Now().UTC()
	s.lastMedia, s.updatedAt = now, now
	s.mu.Unlock()
}

func (s *gbLabSession) snapshot() LabSessionSnapshot {
	s.mu.RLock()
	moduleSession := s.moduleSession
	mode := s.request.Mode
	result := LabSessionSnapshot{
		ID: s.id, Identity: s.identity, DeviceID: s.request.DeviceID, ChannelID: s.request.ChannelID,
		StreamKey: s.request.StreamKey, Mode: s.request.Mode, State: s.state, Direction: s.direction,
		LastError: redactedLabError(s.closeErr),
		StartedAt: s.startedAt, UpdatedAt: s.updatedAt, LastMediaAt: s.lastMedia, StoppedAt: s.stoppedAt,
	}
	s.mu.RUnlock()
	result.RTPPacketsSent = s.rtpPacketsSent.Load()
	result.RTPPacketsRecv = s.rtpPacketsRecv.Load()
	result.RTPBytesSent = s.rtpBytesSent.Load()
	result.RTPBytesRecv = s.rtpBytesRecv.Load()
	result.RTCPPacketsSent = s.rtcpPacketsSent.Load()
	result.RTCPPacketsRecv = s.rtcpPacketsRecv.Load()
	if mode == LabModePublish && moduleSession != nil {
		if receiver := moduleSession.Snapshot().Receiver; receiver != nil {
			result.RTCPPacketsRecv = receiver.RTCPPacketsReceived()
		}
	}
	result.PSFramesSent = s.psFramesSent.Load()
	result.PSFramesRecv = s.psFramesRecv.Load()
	result.AudioFramesSent = s.audioFramesSent.Load()
	result.AudioFramesRecv = s.audioFramesRecv.Load()
	result.VideoFramesSent = s.videoFramesSent.Load()
	result.VideoFramesRecv = s.videoFramesRecv.Load()
	if mode == LabModeReceive && moduleSession != nil {
		if sender := moduleSession.Snapshot().Sender; sender != nil {
			result.RTPPacketsSent = sender.rtpPackets.Load()
			result.RTPBytesSent = sender.rtpBytes.Load()
			result.RTCPPacketsSent = sender.rtcpPackets.Load()
			result.PSFramesSent = sender.mediaFrames.Load()
			result.AudioFramesSent = sender.audioFrames.Load()
			result.VideoFramesSent = sender.videoFrames.Load()
		}
	}
	return result
}

func newGBLabRequest(method sip.RequestMethod, recipient sip.Uri, user, domain, callID string) *sip.Request {
	req := sip.NewRequest(method, recipient)
	req.AppendHeader(&sip.FromHeader{Address: sip.Uri{Scheme: "sip", User: user, Host: domain}, Params: sip.NewParams()})
	req.AppendHeader(&sip.ToHeader{Address: recipient})
	callIDHeader := sip.CallIDHeader(callID)
	req.AppendHeader(&callIDHeader)
	return req
}

func sendGBLabInvite(ctx context.Context, client *sipgo.Client, invite *sip.Request, onConnection ...func(sip.Connection)) (*sip.Response, error) {
	return sendGBLabFinal(ctx, client, invite, true, onConnection...)
}

func sendGBLabRequest(ctx context.Context, client *sipgo.Client, request *sip.Request, onConnection ...func(sip.Connection)) (*sip.Response, error) {
	return sendGBLabFinal(ctx, client, request, false, onConnection...)
}

func sendGBLabFinal(ctx context.Context, client *sipgo.Client, request *sip.Request, invite bool, onConnection ...func(sip.Connection)) (*sip.Response, error) {
	tx, err := client.TransactionRequest(ctx, request)
	if err != nil {
		return nil, err
	}
	observeGBSIPConnection(tx, onConnection)
	defer tx.Terminate()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case response, ok := <-tx.Responses():
			if !ok {
				return nil, errors.New("GB28181 lab SIP transaction closed")
			}
			if response.StatusCode < 200 {
				continue
			}
			if response.StatusCode >= 300 {
				return nil, fmt.Errorf("GB28181 lab %s rejected: %d %s", request.Method, response.StatusCode, response.Reason)
			}
			return response, nil
		}
	}
}

func sendGBLabACK(ctx context.Context, client *sipgo.Client, invite *sip.Request, response *sip.Response, onConnection ...func(sip.Connection)) error {
	ack := newGBLabDialogRequest(sip.ACK, invite, response, 1)
	if err := sipgo.ClientRequestBuild(client, ack); err != nil {
		return err
	}
	conn, err := client.TransportLayer().ClientRequestConnection(ctx, ack)
	if err != nil {
		return err
	}
	if len(onConnection) > 0 && onConnection[0] != nil {
		onConnection[0](conn)
	}
	defer conn.TryClose()
	return conn.WriteMsg(ack)
}

func sendGBLabBYE(ctx context.Context, client *sipgo.Client, invite *sip.Request, response *sip.Response, onConnection ...func(sip.Connection)) error {
	bye := newGBLabDialogRequest(sip.BYE, invite, response, 2)
	response, err := sendGBLabRequest(ctx, client, bye, onConnection...)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("GB28181 lab BYE rejected: %d %s", response.StatusCode, response.Reason)
	}
	return nil
}

func observeGBSIPConnection(tx sip.ClientTransaction, observers []func(sip.Connection)) {
	if len(observers) == 0 || observers[0] == nil {
		return
	}
	connectionProvider, ok := tx.(interface{ Connection() sip.Connection })
	if ok {
		observers[0](connectionProvider.Connection())
	}
}

func newGBLabDialogRequest(method sip.RequestMethod, invite *sip.Request, response *sip.Response, cseq uint32) *sip.Request {
	req := sip.NewRequest(method, invite.Recipient)
	if from := invite.From(); from != nil {
		req.AppendHeader(sip.HeaderClone(from))
	}
	if to := response.To(); to != nil {
		req.AppendHeader(sip.HeaderClone(to))
	}
	if callID := invite.CallID(); callID != nil {
		req.AppendHeader(sip.HeaderClone(callID))
	}
	req.AppendHeader(&sip.CSeqHeader{SeqNo: cseq, MethodName: method})
	req.SetTransport(invite.Transport())
	return req
}

func buildGBLabSDP(port int, direction string) []byte {
	return []byte(fmt.Sprintf("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=LiveForge GB28181 lab\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=video %d RTP/AVP 96\r\na=%s\r\na=rtpmap:96 PS/90000\r\n", port, direction))
}

func listenGBLabUDPPair() (*net.UDPConn, *net.UDPConn, error) {
	for attempt := 0; attempt < 20; attempt++ {
		rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
		if err != nil {
			return nil, nil, err
		}
		port := rtpConn.LocalAddr().(*net.UDPAddr).Port
		rtcpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port + 1})
		if err == nil {
			return rtpConn, rtcpConn, nil
		}
		_ = rtpConn.Close()
	}
	return nil, nil, errors.New("could not allocate GB28181 lab RTP/RTCP pair")
}

func sipAddress(address string) (string, int, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, err
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	var port int
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil || port <= 0 {
		return "", 0, fmt.Errorf("invalid SIP service port %q", portText)
	}
	return host, port, nil
}

func joinSIPResponseError(err error, response *sip.Response) error {
	if err != nil {
		return err
	}
	if response == nil {
		return errors.New("empty SIP response")
	}
	return fmt.Errorf("SIP response %d %s", response.StatusCode, response.Reason)
}
