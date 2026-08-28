package sipgateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/util"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
)

// CallSession manages a single SIP call with RTP bridging to/from a stream.
type CallSession struct {
	callID    string
	streamKey string
	codec     negotiatedCodec
	direction string // "inbound" or "outbound"

	rtpPort    int
	rtcpPort   int
	conn       *net.UDPConn
	rtcpConn   *net.UDPConn
	remoteAddr *net.UDPAddr

	stream            *core.Stream
	startupSnapshot   core.StreamStartupSnapshot
	releaseSubscriber func()
	publisher         *sipPublisher
	dialog            *dialogTeardown
	metrics           *gatewayMetrics

	lifecycleMu     sync.Mutex
	mu              sync.RWMutex
	state           CallState
	startedAt       time.Time
	lastError       string
	publishStarted  atomic.Bool
	lastRTPUnixNano atomic.Int64
	rtpPacketsSent  atomic.Uint64
	rtpPacketsRecv  atomic.Uint64
	rtpBytesSent    atomic.Uint64
	rtpBytesRecv    atomic.Uint64
	rtcpPacketsRecv atomic.Uint64
	rtpIdleTimeout  time.Duration
	onTerminate     func(*CallSession, CallState, error)
	established     atomic.Bool
	terminateOnce   sync.Once
	stopOnce        sync.Once
	rtcpSender      rtcpSenderState
	rtpBuffer       []byte
	transcodedAudio *util.RingReader[*avframe.AVFrame]
	releaseAudio    func()
	video           *sipVideoTrack
	closed          chan struct{}
}

type sipVideoTrack struct {
	codec      negotiatedCodec
	rtpPort    int
	rtcpPort   int
	conn       *net.UDPConn
	rtcpConn   *net.UDPConn
	remoteAddr *net.UDPAddr
	rtcpSender rtcpSenderState
}

const sipSenderReportInterval = time.Second

type rtcpSenderState struct {
	mu          sync.Mutex
	ssrc        uint32
	packetCount uint32
	octetCount  uint32
	lastReport  time.Time
}

type senderReportSnapshot struct {
	ssrc        uint32
	rtpTime     uint32
	packetCount uint32
	octetCount  uint32
	ntpTime     uint64
}

func (s *rtcpSenderState) recordPacket(packet *pionrtp.Packet, now time.Time) (senderReportSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ssrc = packet.SSRC
	s.packetCount++
	s.octetCount += uint32(len(packet.Payload))
	if !s.lastReport.IsZero() && now.Sub(s.lastReport) < sipSenderReportInterval {
		return senderReportSnapshot{}, false
	}
	s.lastReport = now
	return senderReportSnapshot{
		ssrc:        s.ssrc,
		rtpTime:     packet.Timestamp,
		packetCount: s.packetCount,
		octetCount:  s.octetCount,
		ntpTime:     sipNTPTime(now),
	}, true
}

type dialogTeardown struct {
	dialog    inviteDialog
	once      sync.Once
	closeOnce sync.Once
	err       error
}

func newDialogTeardown(dialog inviteDialog) *dialogTeardown {
	return &dialogTeardown{dialog: dialog}
}

func (d *dialogTeardown) teardown() error {
	if d == nil || d.dialog == nil {
		return nil
	}
	d.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		d.err = d.dialog.SendBYE(ctx)
		cancel()
		d.close()
	})
	return d.err
}

func (d *dialogTeardown) close() {
	if d == nil || d.dialog == nil {
		return
	}
	d.closeOnce.Do(d.dialog.Close)
}

// abort closes the INVITE transaction and sends BYE if a 2xx established a
// dialog while the setup path was being retired.
func (d *dialogTeardown) abort() error {
	if d == nil || d.dialog == nil {
		return nil
	}
	d.close()
	response := d.dialog.Response()
	if response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		d.once.Do(func() {})
		return nil
	}
	return d.teardown()
}

func (d *dialogTeardown) endedByRemote() {
	if d == nil || d.dialog == nil {
		return
	}
	d.once.Do(func() {})
	d.close()
}

type sipPublisher struct {
	mu   sync.RWMutex
	id   string
	info *avframe.MediaInfo
}

func (p *sipPublisher) ID() string { return p.id }

func (p *sipPublisher) MediaInfo() *avframe.MediaInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.info == nil {
		return nil
	}
	info := *p.info
	info.VideoSequenceHeader = append([]byte(nil), p.info.VideoSequenceHeader...)
	info.AudioSequenceHeader = append([]byte(nil), p.info.AudioSequenceHeader...)
	return &info
}

func (p *sipPublisher) setVideoSequenceHeader(payload []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.info != nil {
		p.info.VideoSequenceHeader = append(p.info.VideoSequenceHeader[:0], payload...)
	}
}

func (p *sipPublisher) Close() error { return nil }

func newCallSession(callID, streamKey string, codec negotiatedCodec, direction string, rtpPort, rtcpPort int) *CallSession {
	return &CallSession{
		callID:         callID,
		streamKey:      streamKey,
		codec:          codec,
		direction:      direction,
		rtpPort:        rtpPort,
		rtcpPort:       rtcpPort,
		state:          CallStateEstablishing,
		startedAt:      time.Now().UTC(),
		rtpIdleTimeout: 30 * time.Second,
		closed:         make(chan struct{}),
	}
}

func (cs *CallSession) configureVideo(codec negotiatedCodec, rtpPort, rtcpPort int, remoteIP string, remotePort int) {
	track := &sipVideoTrack{codec: codec, rtpPort: rtpPort, rtcpPort: rtcpPort}
	if ip := net.ParseIP(remoteIP); ip != nil && remotePort > 0 {
		track.remoteAddr = &net.UDPAddr{IP: ip, Port: remotePort}
	}
	cs.video = track
}

func (cs *CallSession) configureMediaSockets(rtpConn, rtcpConn *net.UDPConn) {
	cs.mu.Lock()
	cs.conn = rtpConn
	cs.rtcpConn = rtcpConn
	cs.mu.Unlock()
}

func (cs *CallSession) configureVideoSockets(rtpConn, rtcpConn *net.UDPConn) {
	cs.mu.Lock()
	if cs.video != nil {
		cs.video.conn = rtpConn
		cs.video.rtcpConn = rtcpConn
	}
	cs.mu.Unlock()
}

func (cs *CallSession) startInbound(stream *core.Stream, remoteIP string, remotePort int) error {
	cs.lifecycleMu.Lock()
	defer cs.lifecycleMu.Unlock()
	select {
	case <-cs.closed:
		return errors.New("call session is terminated")
	default:
	}
	cs.mu.RLock()
	conn, rtcpConn, video := cs.conn, cs.rtcpConn, cs.video
	cs.mu.RUnlock()
	if conn == nil || rtcpConn == nil {
		return errors.New("SIP gateway audio sockets are not reserved")
	}
	if video != nil && (video.conn == nil || video.rtcpConn == nil) {
		return errors.New("SIP gateway video sockets are not reserved")
	}
	publisher := &sipPublisher{
		id: "sip-" + cs.callID,
		info: &avframe.MediaInfo{
			AudioCodec: cs.codec.Codec,
			SampleRate: cs.codec.ClockRate,
			Channels:   1,
		},
	}
	if cs.video != nil {
		publisher.info.VideoCodec = cs.video.codec.Codec
	}
	if err := stream.SetPublisher(publisher); err != nil {
		return fmt.Errorf("set stream publisher: %w", err)
	}
	startup := stream.StartupSnapshot()
	cs.mu.Lock()
	cs.stream = stream
	cs.publisher = publisher
	cs.startupSnapshot = startup
	cs.mu.Unlock()

	cs.mu.Lock()
	if remoteIP != "" && remotePort > 0 {
		cs.remoteAddr = &net.UDPAddr{IP: net.ParseIP(remoteIP), Port: remotePort}
	}
	cs.state = CallStateActive
	cs.mu.Unlock()
	cs.established.Store(true)

	go cs.receiveLoop()
	go cs.receiveInboundRTCPLoop(rtcpConn)
	if cs.video != nil {
		go cs.receiveVideoLoop(cs.video)
		go cs.receiveInboundRTCPLoop(cs.video.rtcpConn)
	}
	return nil
}

// startPublishLifecycle serializes the inbound publish-start event with
// session termination. This prevents a stop event from overtaking a start
// when a call is closed immediately after RTP setup.
func (cs *CallSession) startPublishLifecycle(emit func() error) bool {
	cs.lifecycleMu.Lock()
	defer cs.lifecycleMu.Unlock()
	if cs.publishStarted.Load() {
		return false
	}
	cs.mu.RLock()
	active := cs.state == CallStateActive
	cs.mu.RUnlock()
	if !active {
		return false
	}
	if emit != nil {
		if err := emit(); err != nil {
			return false
		}
	}
	cs.publishStarted.Store(true)
	return true
}

func (cs *CallSession) publishLifecycleStarted() bool {
	return cs.publishStarted.Load()
}

func (cs *CallSession) receiveInboundRTCPLoop(conn *net.UDPConn) {
	buf := make([]byte, 1500)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			return
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-cs.closed:
				return
			default:
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err == nil && len(packets) > 0 {
			cs.rtcpPacketsRecv.Add(uint64(len(packets)))
		}
	}
}

func (cs *CallSession) startOutbound(stream *core.Stream, startupSnapshot core.StreamStartupSnapshot, remoteIP string, remotePort int) error {
	cs.lifecycleMu.Lock()
	defer cs.lifecycleMu.Unlock()
	select {
	case <-cs.closed:
		return errors.New("call session is terminated")
	default:
	}
	cs.mu.RLock()
	conn, rtcpConn, video := cs.conn, cs.rtcpConn, cs.video
	cs.mu.RUnlock()
	if conn == nil || rtcpConn == nil {
		return errors.New("SIP gateway audio sockets are not reserved")
	}
	if video != nil && (video.conn == nil || video.rtcpConn == nil) {
		return errors.New("SIP gateway video sockets are not reserved")
	}
	if !stream.IsPublisherGeneration(startupSnapshot.Generation) {
		return errors.New("stream publisher generation is no longer active")
	}
	var transcodedAudio *util.RingReader[*avframe.AVFrame]
	var releaseAudio func()
	if startupSnapshot.MediaInfo.AudioCodec != cs.codec.Codec {
		manager := stream.TranscodeManager()
		if manager == nil {
			return ErrCodecMismatch
		}
		var err error
		transcodedAudio, releaseAudio, err = manager.GetOrCreateAudioReaderAtFromHistory(cs.codec.Codec, startupSnapshot)
		if err != nil {
			return fmt.Errorf("acquire SIP target audio: %w", err)
		}
	}
	audioOwned := releaseAudio != nil
	defer func() {
		if audioOwned {
			cs.releaseTranscodedAudio(transcodedAudio, releaseAudio)
		}
	}()

	remoteIPAddr := net.ParseIP(remoteIP)
	if remoteIPAddr == nil || remotePort <= 0 {
		return fmt.Errorf("invalid remote RTP address %q:%d", remoteIP, remotePort)
	}

	if video != nil && video.remoteAddr == nil {
		return errors.New("invalid remote video RTP address")
	}

	cs.mu.Lock()
	cs.stream = stream
	cs.startupSnapshot = startupSnapshot
	cs.remoteAddr = &net.UDPAddr{IP: remoteIPAddr, Port: remotePort}
	cs.transcodedAudio = transcodedAudio
	cs.releaseAudio = releaseAudio
	cs.mu.Unlock()

	releaseSubscriber, err := stream.AddSubscriberForGeneration("sipgateway", startupSnapshot.Generation)
	if err != nil {
		return fmt.Errorf("admit SIP gateway subscriber: %w", err)
	}
	cs.mu.Lock()
	cs.releaseSubscriber = releaseSubscriber
	cs.mu.Unlock()

	cs.mu.Lock()
	cs.state = CallStateActive
	cs.mu.Unlock()
	cs.established.Store(true)

	go cs.sendLoop()
	audioOwned = false
	go cs.receiveRTCPLoop()
	if cs.video != nil {
		go cs.receiveVideoRTCPLoop(cs.video)
	}
	return nil
}

func (cs *CallSession) receiveRTCPLoop() {
	defer slog.Info("rtcp receive loop stopped", "module", "sipgateway", "call", cs.callID)

	buf := make([]byte, 1500)
	cs.mu.RLock()
	conn := cs.rtcpConn
	remoteAddr := cs.remoteAddr
	idleTimeout := cs.rtpIdleTimeout
	cs.mu.RUnlock()
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Second
	}
	lastValid := time.Now()

	for {
		if err := conn.SetReadDeadline(lastValid.Add(idleTimeout)); err != nil {
			cs.networkLost(err)
			return
		}
		n, sender, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-cs.closed:
				return
			default:
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				cs.networkLost(fmt.Errorf("RTCP liveness timeout after %s", idleTimeout))
				return
			}
			cs.networkLost(err)
			return
		}
		if remoteAddr != nil && remoteAddr.IP != nil && !sender.IP.Equal(remoteAddr.IP) {
			continue
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err != nil || len(packets) == 0 {
			continue
		}
		cs.rtcpPacketsRecv.Add(uint64(len(packets)))
		lastValid = time.Now()
	}
}

func (cs *CallSession) receiveLoop() {
	defer slog.Info("rtp receive loop stopped", "module", "sipgateway", "call", cs.callID)

	depacketizer := cs.newDepacketizer()
	if depacketizer.inner == nil {
		cs.networkLost(ErrCodecMismatch)
		return
	}
	buf := make([]byte, 2048)
	cs.mu.RLock()
	conn := cs.conn
	idleTimeout := cs.rtpIdleTimeout
	publisher := cs.publisher
	cs.mu.RUnlock()
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Second
	}

	for {
		if err := conn.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
			cs.networkLost(err)
			return
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-cs.closed:
				return
			default:
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				cs.networkLost(fmt.Errorf("RTP idle timeout after %s", idleTimeout))
				return
			}
			slog.Warn("rtp read error", "module", "sipgateway", "call", cs.callID, "error", err)
			cs.networkLost(err)
			return
		}

		if n < 12 {
			continue
		}

		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		cs.recordRTPReceived(n)

		frame, err := depacketizer.depacketize(&pkt)
		if err != nil || frame == nil {
			continue
		}
		frame.DTS = int64(pkt.Timestamp) * 1000 / int64(cs.codec.ClockRate)
		frame.PTS = frame.DTS

		if !cs.stream.WriteFrameForPublisher(publisher, frame) && cs.stream.Publisher() != publisher {
			cs.Close()
			return
		}
	}
}

func (cs *CallSession) receiveVideoLoop(track *sipVideoTrack) {
	depacketizer := &rtp.H264Depacketizer{}
	cs.mu.RLock()
	publisher := cs.publisher
	cs.mu.RUnlock()
	buf := make([]byte, 64*1024)
	for {
		if err := track.conn.SetReadDeadline(time.Now().Add(cs.rtpIdleTimeout)); err != nil {
			cs.networkLost(err)
			return
		}
		n, _, err := track.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-cs.closed:
				return
			default:
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				cs.networkLost(fmt.Errorf("video RTP idle timeout after %s", cs.rtpIdleTimeout))
				return
			}
			cs.networkLost(err)
			return
		}
		var packet pionrtp.Packet
		if packet.Unmarshal(buf[:n]) != nil {
			continue
		}
		cs.recordRTPReceived(n)
		frames, depacketizeErr := rtp.DepacketizeFrames(depacketizer, &packet)
		if depacketizeErr != nil {
			continue
		}
		dts := int64(packet.Timestamp) * 1000 / int64(track.codec.ClockRate)
		for _, frame := range frames {
			if frame == nil {
				continue
			}
			frame.DTS = dts
			frame.PTS = dts
			if frame.FrameType == avframe.FrameTypeSequenceHeader && publisher != nil {
				publisher.setVideoSequenceHeader(frame.Payload)
			}
			if !cs.stream.WriteFrameForPublisher(publisher, frame) && cs.stream.Publisher() != publisher {
				cs.Close()
				return
			}
		}
	}
}

func (cs *CallSession) receiveVideoRTCPLoop(track *sipVideoTrack) {
	buf := make([]byte, 1500)
	lastValid := time.Now()
	for {
		if err := track.rtcpConn.SetReadDeadline(lastValid.Add(cs.rtpIdleTimeout)); err != nil {
			cs.networkLost(err)
			return
		}
		n, sender, err := track.rtcpConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-cs.closed:
				return
			default:
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				cs.networkLost(fmt.Errorf("video RTCP liveness timeout after %s", cs.rtpIdleTimeout))
				return
			}
			cs.networkLost(err)
			return
		}
		if track.remoteAddr != nil && track.remoteAddr.IP != nil && !sender.IP.Equal(track.remoteAddr.IP) {
			continue
		}
		packets, err := rtcp.Unmarshal(buf[:n])
		if err != nil || len(packets) == 0 {
			continue
		}
		cs.rtcpPacketsRecv.Add(uint64(len(packets)))
		lastValid = time.Now()
	}
}

func (cs *CallSession) sendLoop() {
	defer slog.Info("rtp send loop stopped", "module", "sipgateway", "call", cs.callID)

	audioSession := rtp.NewSession(uint8(cs.codec.PT), uint32(cs.codec.ClockRate))
	audioPacketizer, err := rtp.NewPacketizer(cs.codec.Codec)
	if err != nil {
		cs.networkLost(ErrCodecMismatch)
		return
	}
	cs.mu.RLock()
	stream := cs.stream
	releaseSubscriber := cs.releaseSubscriber
	conn := cs.conn
	rtcpConn := cs.rtcpConn
	remoteAddr := cs.remoteAddr
	video := cs.video
	transcodedAudio := cs.transcodedAudio
	releaseAudio := cs.releaseAudio
	cs.mu.RUnlock()
	if transcodedAudio != nil || releaseAudio != nil {
		defer cs.releaseTranscodedAudio(transcodedAudio, releaseAudio)
	}
	var videoSession *rtp.Session
	var videoPacketizer rtp.Packetizer
	if video != nil {
		videoSession = rtp.NewSession(uint8(video.codec.PT), uint32(video.codec.ClockRate))
		videoPacketizer, err = rtp.NewPacketizer(video.codec.Codec)
		if err != nil {
			cs.networkLost(ErrCodecMismatch)
			return
		}
	}

	defer func() {
		if releaseSubscriber != nil {
			releaseSubscriber()
		}
	}()

	readCtx, cancelRead := context.WithCancel(context.Background())
	defer cancelRead()
	go func() {
		select {
		case <-cs.closed:
			cancelRead()
		case <-readCtx.Done():
		}
	}()
	cs.mu.RLock()
	snapshot := cs.startupSnapshot
	cs.mu.RUnlock()
	if !stream.IsPublisherGeneration(snapshot.Generation) {
		cs.ended()
		return
	}
	generationCtx, cancelGeneration := context.WithCancel(readCtx)
	defer cancelGeneration()
	go func() {
		select {
		case <-snapshot.GenerationDone:
			cancelGeneration()
		case <-generationCtx.Done():
		}
	}()

	for _, header := range []*avframe.AVFrame{snapshot.VideoSequenceHeader, snapshot.AudioSequenceHeader} {
		if header == nil ||
			(header.MediaType.IsAudio() && (transcodedAudio != nil || header.Codec != cs.codec.Codec)) ||
			(header.MediaType.IsVideo() && (video == nil || header.Codec != video.codec.Codec)) {
			continue
		}
		if !cs.sendFrame(header, func() rtp.Packetizer {
			if header.MediaType.IsVideo() {
				return videoPacketizer
			}
			return audioPacketizer
		}(), func() *rtp.Session {
			if header.MediaType.IsVideo() {
				return videoSession
			}
			return audioSession
		}(), func() *net.UDPConn {
			if header.MediaType.IsVideo() {
				return video.conn
			}
			return conn
		}(), func() *net.UDPConn {
			if header.MediaType.IsVideo() {
				return video.rtcpConn
			}
			return rtcpConn
		}(), func() *net.UDPAddr {
			if header.MediaType.IsVideo() {
				return video.remoteAddr
			}
			return remoteAddr
		}(), func() *rtcpSenderState {
			if header.MediaType.IsVideo() {
				return &video.rtcpSender
			}
			return &cs.rtcpSender
		}()) {
			return
		}
	}
	for _, frame := range snapshot.ReplayFrames {
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			cs.ended()
			return
		}
		switch {
		case frame.MediaType == avframe.MediaTypeAudio && frame.Codec == cs.codec.Codec:
			if !cs.sendFrame(frame, audioPacketizer, audioSession, conn, rtcpConn, remoteAddr, &cs.rtcpSender) {
				return
			}
		case video != nil && frame.MediaType == avframe.MediaTypeVideo && frame.Codec == video.codec.Codec:
			if !cs.sendFrame(frame, videoPacketizer, videoSession, video.conn, video.rtcpConn, video.remoteAddr, &video.rtcpSender) {
				return
			}
		}
	}

	reader := stream.RingBuffer().NewReaderAt(snapshot.LiveCursor)
	if transcodedAudio != nil {
		cs.sendTranscodedAudioAndVideo(generationCtx, stream, snapshot, reader, transcodedAudio,
			audioPacketizer, audioSession, conn, rtcpConn, remoteAddr,
			videoPacketizer, videoSession, video)
		return
	}

	for {
		frame, ok := reader.ReadContext(generationCtx)
		if !ok {
			cs.ended()
			return
		}
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			cs.ended()
			return
		}

		switch {
		case frame.MediaType == avframe.MediaTypeAudio && frame.Codec == cs.codec.Codec:
			if !cs.sendFrame(frame, audioPacketizer, audioSession, conn, rtcpConn, remoteAddr, &cs.rtcpSender) {
				return
			}
		case video != nil && frame.MediaType == avframe.MediaTypeVideo && frame.Codec == video.codec.Codec:
			if !cs.sendFrame(frame, videoPacketizer, videoSession, video.conn, video.rtcpConn, video.remoteAddr, &video.rtcpSender) {
				return
			}
		}
	}
}

func (cs *CallSession) sendTranscodedAudioAndVideo(
	ctx context.Context,
	stream *core.Stream,
	snapshot core.StreamStartupSnapshot,
	sourceReader, audioReader *util.RingReader[*avframe.AVFrame],
	audioPacketizer rtp.Packetizer,
	audioSession *rtp.Session,
	audioConn, audioRTCPConn *net.UDPConn,
	audioRemote *net.UDPAddr,
	videoPacketizer rtp.Packetizer,
	videoSession *rtp.Session,
	video *sipVideoTrack,
) {
	sourceFrames := make(chan *avframe.AVFrame)
	audioFrames := make(chan *avframe.AVFrame)
	pump := func(reader *util.RingReader[*avframe.AVFrame], output chan<- *avframe.AVFrame) {
		defer close(output)
		for {
			frame, ok := reader.ReadContext(ctx)
			if !ok {
				return
			}
			select {
			case output <- frame:
			case <-ctx.Done():
				return
			}
		}
	}
	go pump(sourceReader, sourceFrames)
	go pump(audioReader, audioFrames)

	for sourceFrames != nil {
		select {
		case frame, ok := <-sourceFrames:
			if !ok {
				sourceFrames = nil
				continue
			}
			if !stream.IsPublisherGeneration(snapshot.Generation) {
				cs.ended()
				return
			}
			if video != nil && frame.MediaType.IsVideo() && frame.Codec == video.codec.Codec {
				if !cs.sendFrame(frame, videoPacketizer, videoSession, video.conn, video.rtcpConn, video.remoteAddr, &video.rtcpSender) {
					return
				}
			}
		case frame, ok := <-audioFrames:
			if !ok {
				if stream.IsPublisherGeneration(snapshot.Generation) {
					cs.networkLost(errors.New("SIP gateway target audio ended"))
				} else {
					cs.ended()
				}
				return
			}
			if frame.FrameType == avframe.FrameTypeSequenceHeader || !frame.MediaType.IsAudio() || frame.Codec != cs.codec.Codec {
				continue
			}
			if ctx.Err() != nil || !stream.IsPublisherGeneration(snapshot.Generation) {
				cs.ended()
				return
			}
			if !cs.sendFrame(frame, audioPacketizer, audioSession, audioConn, audioRTCPConn, audioRemote, &cs.rtcpSender) {
				return
			}
		case <-ctx.Done():
			cs.ended()
			return
		}
	}
	cs.ended()
}

func (cs *CallSession) releaseTranscodedAudio(reader *util.RingReader[*avframe.AVFrame], release func()) {
	if reader != nil {
		reader.Close()
	}
	if release != nil {
		release()
	}
	cs.mu.Lock()
	if cs.transcodedAudio == reader {
		cs.transcodedAudio = nil
		cs.releaseAudio = nil
	}
	cs.mu.Unlock()
}

func (cs *CallSession) sendFrame(frame *avframe.AVFrame, packetizer rtp.Packetizer, session *rtp.Session, conn, rtcpConn *net.UDPConn, remoteAddr *net.UDPAddr, reportState *rtcpSenderState) bool {
	packets, err := packetizer.Packetize(frame, 1400)
	if err != nil || len(packets) == 0 {
		return true
	}
	session.WrapPackets(packets, frame.DTS)
	for _, packet := range packets {
		packetSize := packet.MarshalSize()
		if cap(cs.rtpBuffer) < packetSize {
			cs.rtpBuffer = make([]byte, packetSize)
		}
		data := cs.rtpBuffer[:packetSize]
		encodedSize, marshalErr := packet.MarshalTo(data)
		if marshalErr != nil {
			continue
		}
		n, writeErr := conn.WriteToUDP(data[:encodedSize], remoteAddr)
		if writeErr != nil {
			cs.networkLost(writeErr)
			return false
		}
		cs.recordRTPSent(n)
		cs.sendSenderReport(rtcpConn, remoteAddr, packet, reportState)
	}
	return true
}

func (cs *CallSession) sendSenderReport(rtcpConn *net.UDPConn, remoteAddr *net.UDPAddr, packet *pionrtp.Packet, state *rtcpSenderState) {
	if rtcpConn == nil || remoteAddr == nil || remoteAddr.Port >= 65535 || packet == nil || state == nil {
		return
	}
	report, due := state.recordPacket(packet, time.Now())
	if !due {
		return
	}
	data, err := (&rtcp.SenderReport{
		SSRC:        report.ssrc,
		NTPTime:     report.ntpTime,
		RTPTime:     report.rtpTime,
		PacketCount: report.packetCount,
		OctetCount:  report.octetCount,
	}).Marshal()
	if err != nil {
		return
	}
	_, _ = rtcpConn.WriteToUDP(data, &net.UDPAddr{IP: remoteAddr.IP, Port: remoteAddr.Port + 1})
}

func sipNTPTime(now time.Time) uint64 {
	const ntpEpochOffset = 2208988800
	seconds := uint64(now.Unix() + ntpEpochOffset)
	fraction := uint64(now.Nanosecond()) * (uint64(1) << 32) / uint64(time.Second)
	return seconds<<32 | fraction
}

// Close terminates a session and notifies its gateway owner exactly once.
func (cs *CallSession) Close() {
	cs.terminate(CallStateEnded, nil, true)
}

func (cs *CallSession) ended() {
	cs.terminate(CallStateEnded, nil, true)
}

func (cs *CallSession) networkLost(err error) {
	cs.terminate(CallStateNetworkLost, err, true)
}

func (cs *CallSession) terminate(state CallState, err error, notify bool) bool {
	terminated := false
	var callback func(*CallSession, CallState, error)
	cs.lifecycleMu.Lock()
	cs.terminateOnce.Do(func() {
		terminated = true
		cs.mu.Lock()
		cs.state = state
		if err != nil {
			cs.lastError = redactedTerminalError(err)
		}
		callback = cs.onTerminate
		cs.mu.Unlock()
		cs.stop()
		if state == CallStateNetworkLost && cs.metrics != nil {
			cs.metrics.networkFailures.Add(1)
		}
	})
	cs.lifecycleMu.Unlock()
	if terminated && notify && callback != nil {
		callback(cs, state, err)
	}
	return terminated
}

func (cs *CallSession) stop() {
	cs.stopOnce.Do(func() {
		close(cs.closed)
		cs.mu.RLock()
		conn := cs.conn
		rtcpConn := cs.rtcpConn
		video := cs.video
		cs.mu.RUnlock()
		if conn != nil {
			_ = conn.Close()
		}
		if rtcpConn != nil {
			_ = rtcpConn.Close()
		}
		if video != nil {
			if video.conn != nil {
				_ = video.conn.Close()
			}
			if video.rtcpConn != nil {
				_ = video.rtcpConn.Close()
			}
		}
	})
}

func (cs *CallSession) recordRTPReceived(bytes int) {
	cs.rtpPacketsRecv.Add(1)
	cs.rtpBytesRecv.Add(uint64(bytes))
	cs.lastRTPUnixNano.Store(time.Now().UnixNano())
	if cs.metrics != nil {
		cs.metrics.rtpPacketsRecv.Add(1)
		cs.metrics.rtpBytesRecv.Add(uint64(bytes))
	}
}

func (cs *CallSession) recordRTPSent(bytes int) {
	cs.rtpPacketsSent.Add(1)
	cs.rtpBytesSent.Add(uint64(bytes))
	cs.lastRTPUnixNano.Store(time.Now().UnixNano())
	if cs.metrics != nil {
		cs.metrics.rtpPacketsSent.Add(1)
		cs.metrics.rtpBytesSent.Add(uint64(bytes))
	}
}

func (cs *CallSession) snapshot() CallSnapshot {
	cs.mu.RLock()
	remoteAddress := ""
	if cs.remoteAddr != nil {
		remoteAddress = cs.remoteAddr.String()
	}
	snapshot := CallSnapshot{
		CallID:        cs.callID,
		Direction:     cs.direction,
		StreamKey:     cs.streamKey,
		Codec:         cs.codec.EncodingName,
		RTPPort:       cs.rtpPort,
		RTCPPort:      cs.rtcpPort,
		RemoteAddress: remoteAddress,
		StartedAt:     cs.startedAt,
		State:         cs.state,
		LastError:     cs.lastError,
	}
	if cs.video != nil {
		snapshot.VideoCodec = cs.video.codec.EncodingName
		snapshot.VideoRTPPort = cs.video.rtpPort
		snapshot.VideoRTCPPort = cs.video.rtcpPort
	}
	cs.mu.RUnlock()

	if lastRTP := cs.lastRTPUnixNano.Load(); lastRTP != 0 {
		snapshot.LastRTPAt = time.Unix(0, lastRTP).UTC()
	}
	snapshot.RTPPacketsSent = cs.rtpPacketsSent.Load()
	snapshot.RTPPacketsRecv = cs.rtpPacketsRecv.Load()
	snapshot.RTPBytesSent = cs.rtpBytesSent.Load()
	snapshot.RTPBytesRecv = cs.rtpBytesRecv.Load()
	snapshot.RTCPPacketsRecv = cs.rtcpPacketsRecv.Load()
	return snapshot
}

type audioDepacketizer struct {
	codec avframe.CodecType
	inner interface {
		Depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error)
	}
}

func (cs *CallSession) newDepacketizer() *audioDepacketizer {
	d := &audioDepacketizer{codec: cs.codec.Codec}
	switch cs.codec.Codec {
	case avframe.CodecG711U:
		d.inner = &rtp.G711Depacketizer{Codec: avframe.CodecG711U}
	case avframe.CodecG711A:
		d.inner = &rtp.G711Depacketizer{Codec: avframe.CodecG711A}
	case avframe.CodecOpus:
		d.inner = &rtp.OpusDepacketizer{}
	case avframe.CodecG722:
		d.inner = &rtp.G722Depacketizer{}
	}
	return d
}

func (d *audioDepacketizer) depacketize(pkt *pionrtp.Packet) (*avframe.AVFrame, error) {
	if d.inner == nil {
		return nil, ErrCodecMismatch
	}
	return d.inner.Depacketize(pkt)
}

type audioPacketizer struct {
	inner interface {
		Packetize(frame *avframe.AVFrame, mtu int) ([]*pionrtp.Packet, error)
	}
}

func (cs *CallSession) newPacketizer() *audioPacketizer {
	p := &audioPacketizer{}
	switch cs.codec.Codec {
	case avframe.CodecG711U, avframe.CodecG711A:
		p.inner = &rtp.G711Packetizer{}
	case avframe.CodecOpus:
		p.inner = &rtp.OpusPacketizer{}
	case avframe.CodecG722:
		p.inner = &rtp.G722Packetizer{}
	}
	return p
}

func (p *audioPacketizer) packetize(frame *avframe.AVFrame) ([]*pionrtp.Packet, error) {
	if p.inner == nil {
		return nil, ErrCodecMismatch
	}
	return p.inner.Packetize(frame, 1400)
}
