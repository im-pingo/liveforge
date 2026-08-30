package gb28181

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	"github.com/im-pingo/liveforge/pkg/portalloc"
	"github.com/im-pingo/liveforge/pkg/util"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
)

const (
	gbOutboundMTU                  = 1200
	gbOutboundSenderReportInterval = time.Second
	gbOutboundLiveMergeHoldback    = 20 * time.Millisecond
)

func supportsGBOutboundMedia(stream *core.Stream, mediaInfo *avframe.MediaInfo) bool {
	if stream == nil || mediaInfo == nil || mediaInfo.VideoCodec != avframe.CodecH264 || mediaInfo.AudioCodec == 0 {
		return false
	}
	if mediaInfo.AudioCodec == avframe.CodecG711A {
		return true
	}
	manager := stream.TranscodeManager()
	return manager != nil && manager.CanTranscode(mediaInfo.AudioCodec, avframe.CodecG711A)
}

func bindGBGeneration(parent context.Context, snapshot core.StreamStartupSnapshot) (context.Context, context.CancelFunc) {
	bound, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-snapshot.GenerationDone:
			cancel()
		case <-bound.Done():
		}
	}()
	return bound, cancel
}

type outboundMediaSession struct {
	ctx    context.Context
	cancel context.CancelFunc
	stream *core.Stream

	rtpConn      *net.UDPConn
	rtcpConn     *net.UDPConn
	remoteRTP    *net.UDPAddr
	remoteRTCP   *net.UDPAddr
	ssrc         uint32
	sequence     uint16
	snapshot     core.StreamStartupSnapshot
	rtpBuffer    []byte
	newPSMuxer   func() *ps.Muxer
	pumpObserver *gbMediaPumpObserver

	closeOnce    sync.Once
	subOnce      sync.Once
	audioOnce    sync.Once
	wg           sync.WaitGroup
	done         chan error
	admitted     bool
	release      func()
	audio        *util.RingReader[*avframe.AVFrame]
	releaseAudio func()
	rtcpMu       sync.Mutex
	lastRTCP     time.Time

	rtpPackets      atomic.Uint64
	rtpBytes        atomic.Uint64
	rtpPayloadBytes atomic.Uint64
	rtcpPackets     atomic.Uint64
	mediaFrames     atomic.Uint64
	audioFrames     atomic.Uint64
	videoFrames     atomic.Uint64
}

type gbVideoRecovery struct {
	waiting        bool
	sequenceHeader *avframe.AVFrame
}

type gbMediaReader string

const (
	gbMediaReaderSource      gbMediaReader = "source"
	gbMediaReaderTargetAudio gbMediaReader = "target_audio"
)

type gbMediaRead struct {
	reader gbMediaReader
	ring   *util.RingReader[*avframe.AVFrame]
	ready  bool
	resume chan struct{}
	result util.RingReadResult[*avframe.AVFrame]
}

type gbMediaPumpObserver struct {
	started         func(gbMediaReader)
	beforeRead      func(gbMediaReader)
	read            func(gbMediaReader, util.RingReadResult[*avframe.AVFrame])
	queued          func(gbMediaReader)
	pending         func(gbMediaReader, *avframe.AVFrame)
	holdbackStarted func(time.Time)
	holdbackFired   func(time.Time)
	exiting         func(gbMediaReader)
	exited          func(gbMediaReader)
	joining         func()
	joined          func()
}

func (r *gbVideoRecovery) restart() {
	r.waiting = true
	r.sequenceHeader = nil
}

func (r *gbVideoRecovery) accept(frame *avframe.AVFrame) (header, media *avframe.AVFrame) {
	if frame == nil {
		return nil, nil
	}
	if !r.waiting {
		return nil, frame
	}
	if frame.FrameType == avframe.FrameTypeSequenceHeader {
		r.sequenceHeader = frame
		return nil, nil
	}
	if !frame.FrameType.IsKeyframe() || r.sequenceHeader == nil {
		return nil, nil
	}
	header = r.sequenceHeader
	r.waiting = false
	r.sequenceHeader = nil
	return header, frame
}

func newOutboundMediaSession(stream *core.Stream, rtpPort, rtcpPort int) (*outboundMediaSession, error) {
	rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: rtpPort})
	if err != nil {
		return nil, err
	}
	rtcpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: rtcpPort})
	if err != nil {
		_ = rtpConn.Close()
		return nil, err
	}
	session, err := newOutboundMediaSessionWithSockets(stream, rtpConn, rtcpConn)
	if err != nil {
		_ = rtpConn.Close()
		_ = rtcpConn.Close()
		return nil, err
	}
	return session, nil
}

func newOutboundMediaSessionFromBoundPair(stream *core.Stream, pair *portalloc.BoundUDPPair) (*outboundMediaSession, error) {
	if pair == nil || pair.RTPConn == nil || pair.RTCPConn == nil {
		return nil, errors.New("bound outbound RTP/RTCP pair is incomplete")
	}
	rtpAddr, rtpOK := pair.RTPConn.LocalAddr().(*net.UDPAddr)
	rtcpAddr, rtcpOK := pair.RTCPConn.LocalAddr().(*net.UDPAddr)
	if !rtpOK || !rtcpOK || rtpAddr.Port != pair.RTPPort || rtcpAddr.Port != pair.RTCPPort || pair.RTCPPort != pair.RTPPort+1 {
		return nil, errors.New("bound outbound RTP/RTCP pair ports are inconsistent")
	}
	return newOutboundMediaSessionWithSockets(stream, pair.RTPConn, pair.RTCPConn)
}

func newOutboundMediaSessionWithSockets(stream *core.Stream, rtpConn, rtcpConn *net.UDPConn) (*outboundMediaSession, error) {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &outboundMediaSession{
		ctx:      ctx,
		cancel:   cancel,
		stream:   stream,
		rtpConn:  rtpConn,
		rtcpConn: rtcpConn,
		ssrc:     binary.BigEndian.Uint32(random[:]),
		done:     make(chan error, 1),
	}, nil
}

func (s *outboundMediaSession) setRemote(address *net.UDPAddr) error {
	if address == nil || address.IP == nil || address.Port <= 0 || address.Port >= 65535 {
		return errors.New("invalid GB28181 outbound media address")
	}
	s.remoteRTP = address
	s.remoteRTCP = &net.UDPAddr{IP: address.IP, Port: address.Port + 1}
	return nil
}

func (s *outboundMediaSession) admit() error {
	release, err := s.stream.AddSubscriberForGeneration("gb28181", s.snapshot.Generation)
	if err != nil {
		return err
	}
	s.admitted = true
	s.release = release
	return nil
}

func (s *outboundMediaSession) configureAudio() error {
	if s.snapshot.MediaInfo.AudioCodec == avframe.CodecG711A {
		return nil
	}
	manager := s.stream.TranscodeManager()
	if manager == nil || !manager.CanTranscode(s.snapshot.MediaInfo.AudioCodec, avframe.CodecG711A) {
		return errors.New("GB28181 outbound media requires G.711A-compatible audio")
	}
	reader, release, err := manager.GetOrCreateAudioReaderAtFromHistory(avframe.CodecG711A, s.snapshot)
	if err != nil {
		return fmt.Errorf("acquire GB28181 target audio: %w", err)
	}
	s.audio = reader
	s.releaseAudio = release
	return nil
}

func (s *outboundMediaSession) start() {
	s.wg.Add(1)
	go s.run()
}

func (s *outboundMediaSession) run() {
	defer s.releaseAudioReader()
	err := s.runMedia()
	s.releaseSubscriber()
	s.done <- err
	s.wg.Done()
}

func (s *outboundMediaSession) runMedia() error {
	readCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	go func() {
		select {
		case <-s.snapshot.GenerationDone:
			cancel()
		case <-readCtx.Done():
		}
	}()
	if !s.stream.IsPublisherGeneration(s.snapshot.Generation) {
		return nil
	}
	muxer := s.freshPSMuxer()
	for _, header := range []*avframe.AVFrame{s.snapshot.VideoSequenceHeader, s.snapshot.AudioSequenceHeader} {
		if header == nil || (s.audio != nil && header.MediaType.IsAudio()) {
			continue
		}
		if err := s.sendFrame(muxer, header); err != nil {
			if readCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("GB28181 outbound media header: %w", err)
		}
	}
	reader := s.stream.RingBuffer().NewReaderAt(s.snapshot.LiveCursor)
	defer reader.Close()
	if s.audio != nil {
		return s.runTranscodedMedia(readCtx, muxer, s.snapshot.ReplayFrames, reader, s.audio)
	}
	for _, frame := range s.snapshot.ReplayFrames {
		if !s.stream.IsPublisherGeneration(s.snapshot.Generation) {
			return nil
		}
		if err := s.sendFrame(muxer, frame); err != nil {
			if readCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("GB28181 outbound media replay: %w", err)
		}
	}
	videoRecovery := gbVideoRecovery{}
	for {
		result := reader.ReadResultContext(readCtx)
		if !result.OK {
			if readCtx.Err() != nil {
				return nil
			}
			return errors.New("GB28181 outbound media source ended")
		}
		if result.Overwritten > 0 {
			reader.AdvanceToLive()
			muxer = s.freshPSMuxer()
			videoRecovery.restart()
			logGBMediaOverwrite("source", "wait_keyframe", result.Overwritten)
			continue
		}
		frame := result.Value
		if !s.stream.IsPublisherGeneration(s.snapshot.Generation) {
			return nil
		}
		if frame == nil ||
			(frame.MediaType == avframe.MediaTypeVideo && frame.Codec != avframe.CodecH264) ||
			(frame.MediaType == avframe.MediaTypeAudio && frame.Codec != avframe.CodecG711A) ||
			(!frame.MediaType.IsVideo() && !frame.MediaType.IsAudio()) {
			continue
		}
		if frame.MediaType.IsVideo() {
			header, media := videoRecovery.accept(frame)
			if media == nil {
				continue
			}
			if header != nil {
				if err := s.sendFrame(muxer, header); err != nil {
					if s.ctx.Err() != nil {
						return nil
					}
					return fmt.Errorf("GB28181 outbound media recovery header: %w", err)
				}
			}
			frame = media
		}
		if err := s.sendFrame(muxer, frame); err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("GB28181 outbound media send: %w", err)
		}
	}
}

func (s *outboundMediaSession) freshPSMuxer() *ps.Muxer {
	if s.newPSMuxer != nil {
		return s.newPSMuxer()
	}
	return ps.NewMuxer()
}

func logGBMediaOverwrite(reader, action string, overwritten int64) {
	slog.Warn("GB28181 media reader continuity lost",
		"module", "gb28181",
		"protocol", "gb28181",
		"reader", reader,
		"overwritten", overwritten,
		"recovery_action", action,
	)
}

func (s *outboundMediaSession) runTranscodedMedia(
	ctx context.Context,
	muxer *ps.Muxer,
	replayFrames []*avframe.AVFrame,
	sourceReader, audioReader *util.RingReader[*avframe.AVFrame],
) error {
	replay, err := s.sendTranscodedReplay(ctx, muxer, replayFrames, audioReader)
	if err != nil {
		return err
	}

	pumpCtx, cancelPumps := context.WithCancel(ctx)
	events := make(chan gbMediaRead, 2)
	observer := s.pumpObserver
	var pumps sync.WaitGroup
	pump := func(kind gbMediaReader, reader *util.RingReader[*avframe.AVFrame]) {
		defer func() {
			if observer != nil && observer.exiting != nil {
				observer.exiting(kind)
			}
			if observer != nil && observer.exited != nil {
				observer.exited(kind)
			}
			pumps.Done()
		}()
		if observer != nil && observer.started != nil {
			observer.started(kind)
		}
		for {
			if observer != nil && observer.beforeRead != nil {
				observer.beforeRead(kind)
			}
			ready := reader.WaitContext(pumpCtx)
			if !ready && pumpCtx.Err() != nil {
				return
			}
			event := gbMediaRead{reader: kind, ring: reader, ready: ready}
			if ready {
				event.resume = make(chan struct{})
			}
			select {
			case events <- event:
			case <-pumpCtx.Done():
				return
			}
			if observer != nil && observer.queued != nil {
				observer.queued(kind)
			}
			if !ready {
				return
			}
			select {
			case <-event.resume:
			case <-pumpCtx.Done():
				return
			}
		}
	}
	pumps.Add(2)
	go pump(gbMediaReaderSource, sourceReader)
	go pump(gbMediaReaderTargetAudio, audioReader)
	defer func() {
		cancelPumps()
		if observer != nil && observer.joining != nil {
			observer.joining()
		}
		pumps.Wait()
		if observer != nil && observer.joined != nil {
			observer.joined()
		}
	}()

	var pendingVideo *avframe.AVFrame
	pendingAudio := replay.pendingAudio
	lastSentDTS, hasLastSentDTS := replay.lastSentDTS, replay.hasLastSentDTS
	videoRecovery := gbVideoRecovery{}
	holdback := time.NewTimer(gbOutboundLiveMergeHoldback)
	if !holdback.Stop() {
		<-holdback.C
	}
	defer holdback.Stop()
	var holdbackC <-chan time.Time
	var holdbackDeadline time.Time
	stopHoldback := func() {
		if holdbackC == nil {
			return
		}
		if !holdback.Stop() {
			select {
			case <-holdback.C:
			default:
			}
		}
		holdbackC = nil
		holdbackDeadline = time.Time{}
	}
	startHoldback := func() {
		stopHoldback()
		holdbackDeadline = time.Now().Add(gbOutboundLiveMergeHoldback)
		holdback.Reset(time.Until(holdbackDeadline))
		holdbackC = holdback.C
		if observer != nil && observer.holdbackStarted != nil {
			observer.holdbackStarted(holdbackDeadline)
		}
	}
	sendLive := func(frame *avframe.AVFrame, reader gbMediaReader) error {
		// Once sparse-track output has advanced the shared PS/RTP clock, a
		// strictly older resumed frame cannot be emitted without going backward.
		if hasLastSentDTS && frame.DTS < lastSentDTS {
			return nil
		}
		if ctx.Err() != nil || !s.stream.IsPublisherGeneration(s.snapshot.Generation) {
			return nil
		}
		if reader == gbMediaReaderSource {
			header, media := videoRecovery.accept(frame)
			if media == nil {
				return nil
			}
			if header != nil {
				if err := s.sendTranscodedFrame(ctx, muxer, header, "recovery video header"); err != nil {
					return err
				}
			}
			frame = media
		}
		if err := s.sendTranscodedFrame(ctx, muxer, frame, string(reader)); err != nil {
			return err
		}
		lastSentDTS = frame.DTS
		hasLastSentDTS = true
		return nil
	}

	for {
		var event gbMediaRead
		haveEvent := false
		select {
		case event = <-events:
			haveEvent = true
		default:
		}
		if !haveEvent {
			if pendingVideo != nil && pendingAudio != nil {
				stopHoldback()
				if pendingVideo.DTS <= pendingAudio.DTS {
					if err := sendLive(pendingVideo, gbMediaReaderSource); err != nil {
						return err
					}
					pendingVideo = nil
				} else {
					if err := sendLive(pendingAudio, gbMediaReaderTargetAudio); err != nil {
						return err
					}
					pendingAudio = nil
				}
				continue
			}
			if pendingVideo != nil || pendingAudio != nil {
				if holdbackC == nil {
					startHoldback()
				}
			} else {
				stopHoldback()
			}
			select {
			case event = <-events:
				haveEvent = true
			case <-holdbackC:
				deadline := holdbackDeadline
				holdbackC = nil
				holdbackDeadline = time.Time{}
				if observer != nil && observer.holdbackFired != nil {
					observer.holdbackFired(deadline)
				}
				if pendingVideo != nil {
					if err := sendLive(pendingVideo, gbMediaReaderSource); err != nil {
						return err
					}
					pendingVideo = nil
				} else if pendingAudio != nil {
					if err := sendLive(pendingAudio, gbMediaReaderTargetAudio); err != nil {
						return err
					}
					pendingAudio = nil
				}
				continue
			case <-ctx.Done():
				return nil
			}
		}

		if event.ready {
			event.result = event.ring.TryReadResult()
			if observer != nil && observer.read != nil {
				observer.read(event.reader, event.result)
			}
			if event.result.Overwritten > 0 {
				event.ring.AdvanceToLive()
			}
			close(event.resume)
		} else if observer != nil && observer.read != nil {
			observer.read(event.reader, event.result)
		}

		if event.result.Overwritten > 0 {
			action := "continue_audio"
			switch event.reader {
			case gbMediaReaderSource:
				pendingVideo = nil
				muxer = s.freshPSMuxer()
				videoRecovery.restart()
				action = "wait_keyframe"
			case gbMediaReaderTargetAudio:
				pendingAudio = nil
			}
			logGBMediaOverwrite(string(event.reader), action, event.result.Overwritten)
			continue
		}
		if !event.result.OK {
			if event.reader == gbMediaReaderTargetAudio &&
				ctx.Err() == nil && s.stream.IsPublisherGeneration(s.snapshot.Generation) {
				return errors.New("GB28181 outbound target audio ended")
			}
			return nil
		}
		frame := event.result.Value
		switch event.reader {
		case gbMediaReaderSource:
			if !isGBOutboundVideo(frame) {
				continue
			}
			if pendingVideo != nil {
				stopHoldback()
				if err := sendLive(pendingVideo, gbMediaReaderSource); err != nil {
					return err
				}
			}
			pendingVideo = frame
			if observer != nil && observer.pending != nil {
				observer.pending(event.reader, frame)
			}
		case gbMediaReaderTargetAudio:
			if !isGBOutboundTargetAudio(frame) {
				continue
			}
			if pendingAudio != nil {
				stopHoldback()
				if err := sendLive(pendingAudio, gbMediaReaderTargetAudio); err != nil {
					return err
				}
			}
			pendingAudio = frame
			if observer != nil && observer.pending != nil {
				observer.pending(event.reader, frame)
			}
		default:
			continue
		}
	}
}

type gbTranscodedReplayState struct {
	pendingAudio   *avframe.AVFrame
	lastSentDTS    int64
	hasLastSentDTS bool
}

func (s *outboundMediaSession) sendTranscodedReplay(
	ctx context.Context,
	muxer *ps.Muxer,
	replayFrames []*avframe.AVFrame,
	audioReader *util.RingReader[*avframe.AVFrame],
) (gbTranscodedReplayState, error) {
	var state gbTranscodedReplayState
	send := func(frame *avframe.AVFrame, kind string) error {
		if err := s.sendTranscodedFrame(ctx, muxer, frame, kind); err != nil {
			return err
		}
		state.lastSentDTS = frame.DTS
		state.hasLastSentDTS = true
		return nil
	}
	var videos []*avframe.AVFrame
	var replayEnd int64
	hasReplay := false
	for _, frame := range replayFrames {
		if frame == nil || (!frame.MediaType.IsVideo() && !frame.MediaType.IsAudio()) {
			continue
		}
		if !hasReplay || frame.DTS > replayEnd {
			replayEnd = frame.DTS
		}
		hasReplay = true
		if isGBOutboundVideo(frame) {
			videos = append(videos, frame)
		}
	}
	if !hasReplay {
		return state, nil
	}

	videoIndex := 0
	for {
		result := readGBOutboundTargetAudio(ctx, audioReader)
		if !result.OK {
			if ctx.Err() != nil || !s.stream.IsPublisherGeneration(s.snapshot.Generation) {
				return state, nil
			}
			return state, errors.New("GB28181 outbound target audio ended during replay")
		}
		if result.Overwritten > 0 {
			audioReader.AdvanceToLive()
			logGBMediaOverwrite(string(gbMediaReaderTargetAudio), "continue_audio", result.Overwritten)
			continue
		}
		audio := result.Value
		for videoIndex < len(videos) && videos[videoIndex].DTS <= audio.DTS {
			if err := send(videos[videoIndex], "replay video"); err != nil {
				return state, err
			}
			videoIndex++
		}
		if audio.DTS > replayEnd {
			for videoIndex < len(videos) {
				if err := send(videos[videoIndex], "replay video"); err != nil {
					return state, err
				}
				videoIndex++
			}
			state.pendingAudio = audio
			return state, nil
		}
		if err := send(audio, "replay audio"); err != nil {
			return state, err
		}
		if audio.DTS >= replayEnd {
			for videoIndex < len(videos) {
				if err := send(videos[videoIndex], "replay video"); err != nil {
					return state, err
				}
				videoIndex++
			}
			return state, nil
		}
	}
}

func (s *outboundMediaSession) sendTranscodedFrame(ctx context.Context, muxer *ps.Muxer, frame *avframe.AVFrame, kind string) error {
	if ctx.Err() != nil || !s.stream.IsPublisherGeneration(s.snapshot.Generation) {
		return nil
	}
	if err := s.sendFrame(muxer, frame); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("GB28181 outbound media %s: %w", kind, err)
	}
	return nil
}

func readGBOutboundTargetAudio(ctx context.Context, reader *util.RingReader[*avframe.AVFrame]) util.RingReadResult[*avframe.AVFrame] {
	for {
		result := reader.ReadResultContext(ctx)
		if !result.OK || result.Overwritten > 0 {
			return result
		}
		if isGBOutboundTargetAudio(result.Value) {
			return result
		}
	}
}

func isGBOutboundVideo(frame *avframe.AVFrame) bool {
	return frame != nil && frame.MediaType.IsVideo() && frame.Codec == avframe.CodecH264
}

func isGBOutboundTargetAudio(frame *avframe.AVFrame) bool {
	return frame != nil && frame.FrameType != avframe.FrameTypeSequenceHeader &&
		frame.MediaType.IsAudio() && frame.Codec == avframe.CodecG711A
}

func (s *outboundMediaSession) sendFrame(muxer *ps.Muxer, frame *avframe.AVFrame) error {
	data, err := muxer.Pack(frame)
	if err != nil {
		return err
	}
	timestamp := uint32(frame.DTS * 90)
	for offset := 0; offset < len(data); {
		end := offset + gbOutboundMTU
		if end > len(data) {
			end = len(data)
		}
		packet := pionrtp.Packet{Header: pionrtp.Header{
			Version:        2,
			PayloadType:    labRTPPayloadType,
			SequenceNumber: s.sequence,
			Timestamp:      timestamp,
			SSRC:           s.ssrc,
			Marker:         end == len(data),
		}, Payload: data[offset:end]}
		packetSize := packet.MarshalSize()
		if cap(s.rtpBuffer) < packetSize {
			s.rtpBuffer = make([]byte, packetSize)
		}
		encoded := s.rtpBuffer[:packetSize]
		encodedSize, err := packet.MarshalTo(encoded)
		if err != nil {
			return err
		}
		n, err := s.rtpConn.WriteToUDP(encoded[:encodedSize], s.remoteRTP)
		if err != nil {
			return err
		}
		s.sequence++
		s.rtpPackets.Add(1)
		s.rtpBytes.Add(uint64(n))
		s.rtpPayloadBytes.Add(uint64(len(packet.Payload)))
		offset = end
	}
	s.mediaFrames.Add(1)
	if frame.MediaType.IsAudio() {
		s.audioFrames.Add(1)
	} else if frame.MediaType.IsVideo() {
		s.videoFrames.Add(1)
	}
	s.sendSenderReport(timestamp, time.Now())
	return nil
}

func (s *outboundMediaSession) sendSenderReport(timestamp uint32, now time.Time) {
	s.rtcpMu.Lock()
	defer s.rtcpMu.Unlock()
	if !s.lastRTCP.IsZero() && now.Sub(s.lastRTCP) < gbOutboundSenderReportInterval {
		return
	}
	report := &rtcp.SenderReport{
		SSRC:        s.ssrc,
		NTPTime:     gbNTPTime(now),
		RTPTime:     timestamp,
		PacketCount: uint32(s.rtpPackets.Load()),
		OctetCount:  uint32(s.rtpPayloadBytes.Load()),
	}
	encoded, err := report.Marshal()
	if err != nil {
		return
	}
	if _, err := s.rtcpConn.WriteToUDP(encoded, s.remoteRTCP); err != nil {
		return
	}
	s.lastRTCP = now
	s.rtcpPackets.Add(1)
}

func (s *outboundMediaSession) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		_ = s.rtpConn.Close()
		_ = s.rtcpConn.Close()
		s.wg.Wait()
		s.releaseAudioReader()
		s.releaseSubscriber()
	})
}

func (s *outboundMediaSession) releaseAudioReader() {
	s.audioOnce.Do(func() {
		if s.audio != nil {
			s.audio.Close()
		}
		if s.releaseAudio != nil {
			s.releaseAudio()
		}
	})
}

func (s *outboundMediaSession) releaseSubscriber() {
	s.subOnce.Do(func() {
		if s.admitted && s.release != nil {
			s.release()
		}
	})
}

func gbNTPTime(now time.Time) uint64 {
	const ntpEpochOffset = 2208988800
	seconds := uint64(now.Unix() + ntpEpochOffset)
	fraction := uint64(now.Nanosecond()) * (uint64(1) << 32) / uint64(time.Second)
	return seconds<<32 | fraction
}

type gbOutboundMediaSource struct {
	stream   *core.Stream
	snapshot core.StreamStartupSnapshot
}

func (m *Module) prepareGBOutboundMedia(ctx context.Context, streamKey string) (gbOutboundMediaSource, error) {
	stream, ok := m.handler.hub.Find(streamKey)
	if !ok {
		return gbOutboundMediaSource{}, fmt.Errorf("%w: receive stream %q", ErrLabInvalidRequest, streamKey)
	}
	pending := stream.StartupSnapshot()
	if pending.Generation == 0 || pending.GenerationDone == nil || pending.PublisherID == "" ||
		!stream.IsPublisherGeneration(pending.Generation) {
		return gbOutboundMediaSource{}, fmt.Errorf("%w: receive stream %q", ErrLabInvalidRequest, streamKey)
	}
	if !supportsGBOutboundMedia(stream, &pending.MediaInfo) {
		return gbOutboundMediaSource{}, fmt.Errorf("%w: receive stream requires H.264 and G.711A-compatible audio", ErrLabInvalidRequest)
	}

	snapshot, startupReady := waitGBStartup(ctx, stream, pending)
	if !startupReady {
		if ctx.Err() != nil {
			return gbOutboundMediaSource{}, ctx.Err()
		}
		return gbOutboundMediaSource{}, errors.New("GB28181 outbound media source generation is not ready")
	}
	if !supportsGBOutboundMedia(stream, &snapshot.MediaInfo) {
		return gbOutboundMediaSource{}, fmt.Errorf("%w: receive stream requires H.264 and G.711A-compatible audio", ErrLabInvalidRequest)
	}
	return gbOutboundMediaSource{stream: stream, snapshot: snapshot}, nil
}

func (m *Module) startOutboundMedia(ctx context.Context, device *Device, channelID, streamKey string) (*MediaSession, error) {
	source, err := m.prepareGBOutboundMedia(ctx, streamKey)
	if err != nil {
		return nil, err
	}
	return m.startOutboundMediaFromSource(ctx, device, channelID, streamKey, source)
}

func (m *Module) startOutboundMediaFromSource(
	ctx context.Context,
	device *Device,
	channelID, streamKey string,
	source gbOutboundMediaSource,
) (*MediaSession, error) {
	stream, snapshot := source.stream, source.snapshot
	if stream == nil || !stream.IsPublisherGeneration(snapshot.Generation) {
		return nil, errors.New("GB28181 outbound media source generation ended")
	}
	startupCtx, cancelStartup := bindGBGeneration(ctx, snapshot)
	defer cancelStartup()
	if !supportsGBOutboundMedia(stream, &snapshot.MediaInfo) {
		return nil, fmt.Errorf("%w: receive stream requires H.264 and G.711A-compatible audio", ErrLabInvalidRequest)
	}

	pair, err := m.handler.ports.AllocateBoundUDPPair("udp", nil)
	if err != nil {
		return nil, err
	}
	rtpPort, rtcpPort := pair.RTPPort, pair.RTCPPort
	portsOwned := true
	defer func() {
		if portsOwned {
			m.handler.ports.Free(rtpPort, rtcpPort)
		}
	}()
	pairOwned := true
	defer func() {
		if pairOwned {
			closeBoundUDPPair(pair)
		}
	}()
	sender, err := newOutboundMediaSessionFromBoundPair(stream, pair)
	if err != nil {
		return nil, err
	}
	pairOwned = false
	senderOwned := true
	defer func() {
		if senderOwned {
			sender.close()
		}
	}()
	sender.snapshot = snapshot
	if err := sender.configureAudio(); err != nil {
		return nil, err
	}
	if err := sender.admit(); err != nil {
		return nil, fmt.Errorf("GB28181 outbound subscriber admission: %w", err)
	}

	remoteIP := extractIP(device.RemoteAddr)
	remotePort := parsePort(device.RemoteAddr)
	request := newGBLabRequest(sip.INVITE, sip.Uri{Scheme: "sip", User: channelID, Host: remoteIP, Port: remotePort}, m.sipService.ServerID(), m.sipService.Domain(), uuid.NewString())
	request.SetBody(buildGBLabSDP(rtpPort, "sendonly"))
	request.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	request.AppendHeader(sip.NewHeader("Subject", fmt.Sprintf("%s:0,%s:0", channelID, m.sipService.ServerID())))
	request.SetTransport("udp")
	dialog, err := sendInvite(startupCtx, m.sipService, m.sendInvite, request)
	if err != nil {
		return nil, err
	}
	dialog = newManagedInviteDialog(dialog)
	dialogOwned := true
	defer func() {
		if dialogOwned {
			cleanupInviteDialog(dialog)
		}
	}()
	select {
	case <-dialog.Done():
	case <-startupCtx.Done():
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return nil, errors.New("GB28181 outbound media source generation ended")
		}
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, errors.New("GB28181 outbound INVITE timeout")
	}
	response := dialog.Response()
	if response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("GB28181 outbound INVITE rejected")
	}
	if !stream.IsPublisherGeneration(snapshot.Generation) {
		terminateAcceptedDialog(dialog)
		return nil, errors.New("GB28181 outbound media source generation ended")
	}
	if err := dialog.SendACK(startupCtx); err != nil {
		terminateAcceptedDialog(dialog)
		if !stream.IsPublisherGeneration(snapshot.Generation) {
			return nil, errors.New("GB28181 outbound media source generation ended")
		}
		return nil, err
	}
	answerPort := parseSDPPort(string(response.Body()))
	if !stream.IsPublisherGeneration(snapshot.Generation) {
		terminateAcceptedDialog(dialog)
		return nil, fmt.Errorf("GB28181 outbound media source generation ended")
	}
	if err := sender.setRemote(&net.UDPAddr{IP: net.ParseIP(remoteIP), Port: answerPort}); err != nil {
		terminateAcceptedDialog(dialog)
		return nil, err
	}
	callID := ""
	if header := response.CallID(); header != nil {
		callID = header.Value()
	}
	if callID == "" {
		callID = uuid.NewString()
	}
	session := &MediaSession{
		ID:         callID,
		DeviceID:   device.DeviceID,
		ChannelID:  channelID,
		StreamKey:  streamKey,
		Direction:  SessionDirectionOutbound,
		LocalPort:  rtpPort,
		RemoteAddr: sender.remoteRTP,
		Transport:  device.Transport,
		State:      SessionStateStreaming,
		Sender:     sender,
		Stream:     stream,
		InviteTx:   dialog,
	}
	m.sessions.Add(session)
	sender.start()
	portsOwned = false
	senderOwned = false
	dialogOwned = false
	return session, nil
}

func waitGBStartup(ctx context.Context, stream *core.Stream, pending core.StreamStartupSnapshot) (core.StreamStartupSnapshot, bool) {
	if pending.Generation == 0 || pending.GenerationDone == nil || pending.PublisherID == "" {
		return core.StreamStartupSnapshot{}, false
	}
	if pending.Ready {
		return pending, stream.IsPublisherGeneration(pending.Generation)
	}

	startupCtx, cancel := bindGBGeneration(ctx, pending)
	snapshot, ok := stream.WaitForStartup(startupCtx)
	cancel()
	if !ok || snapshot.Generation != pending.Generation || snapshot.PublisherID != pending.PublisherID {
		return core.StreamStartupSnapshot{}, false
	}
	return snapshot, stream.IsPublisherGeneration(snapshot.Generation)
}
