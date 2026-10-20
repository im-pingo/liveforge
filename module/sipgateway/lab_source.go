package sipgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/h264"
	mediarp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/util"
	"github.com/pion/rtcp"
)

const sipLabSourceStartupTimeout = 5 * time.Second

// sipLabSource owns only a subscription to one captured publisher generation.
// The target is written exclusively by the normal SIP RTP receiver.
type sipLabSource struct {
	stream   *core.Stream
	snapshot core.StreamStartupSnapshot
	reader   *util.RingReader[*avframe.AVFrame]
	ctx      context.Context
	cancel   context.CancelFunc
	release  func()
	once     sync.Once
}

func (s *sipLabSession) admitPublishSource(ctx context.Context) (*sipLabSource, error) {
	stream, ok := s.gateway.hub.Find(s.request.SourceStreamKey)
	if !ok {
		return nil, fmt.Errorf("%w: publish source %q", ErrStreamNotFound, s.request.SourceStreamKey)
	}
	pending := stream.StartupSnapshot()
	if !stream.IsPublisherGeneration(pending.Generation) || pending.PublisherID == "" {
		return nil, fmt.Errorf("%w: publish source %q", ErrStreamNotFound, s.request.SourceStreamKey)
	}
	wantedAudio := codecAV(strings.ToUpper(strings.TrimSpace(s.request.Codec)))
	startupCtx, cancel := context.WithTimeout(ctx, sipLabSourceStartupTimeout)
	defer cancel()
	boundCtx, unbind := bindSIPGeneration(startupCtx, pending)
	defer unbind()
	wakeup := stream.RingBuffer().NewReaderAt(pending.LiveCursor)
	defer wakeup.Close()
	snapshot := pending
	for {
		if startupCtx.Err() != nil {
			return nil, fmt.Errorf("wait for SIP publish source: %w", startupCtx.Err())
		}
		if snapshot.Generation != pending.Generation || !stream.IsPublisherGeneration(pending.Generation) {
			return nil, errors.New("SIP publish source generation ended during startup")
		}
		if (snapshot.MediaInfo.VideoCodec != 0 && snapshot.MediaInfo.VideoCodec != avframe.CodecH264) ||
			(snapshot.MediaInfo.AudioCodec != 0 && snapshot.MediaInfo.AudioCodec != wantedAudio) {
			return nil, fmt.Errorf("%w: source requires H264 and %s", ErrCodecMismatch, s.request.Codec)
		}
		if snapshot.Ready && snapshot.MediaInfo.VideoCodec == avframe.CodecH264 && snapshot.MediaInfo.AudioCodec == wantedAudio {
			break
		}
		// WHIP discovers its tracks independently. A generally ready stream
		// may still be audio-only until the first video packet arrives.
		if !wakeup.WaitContext(boundCtx) {
			if startupCtx.Err() != nil {
				return nil, fmt.Errorf("wait for SIP publish source: %w", startupCtx.Err())
			}
			return nil, errors.New("SIP publish source generation ended during startup")
		}
		wakeup.AdvanceToLive()
		snapshot = stream.StartupSnapshot()
	}
	if _, err := sipSourceVideoHeader(snapshot.VideoSequenceHeader); err != nil {
		return nil, err
	}
	release, err := stream.AddSubscriberForGeneration("sipgateway", snapshot.Generation)
	if err != nil {
		return nil, fmt.Errorf("admit SIP publish source subscriber: %w", err)
	}
	sourceCtx, sourceCancel := bindSIPGeneration(s.ctx, snapshot)
	return &sipLabSource{stream: stream, snapshot: snapshot,
		reader: stream.RingBuffer().NewReaderAt(snapshot.LiveCursor), ctx: sourceCtx,
		cancel: sourceCancel, release: release}, nil
}

func (source *sipLabSource) active() error {
	if source.ctx.Err() != nil || !source.stream.IsPublisherGeneration(source.snapshot.Generation) {
		return errors.New("SIP publish source generation ended")
	}
	return nil
}

func (source *sipLabSource) close() {
	source.once.Do(func() {
		source.cancel()
		source.reader.Close()
		source.release()
		// Cleanup runs after startup and media workers have joined. Terminal
		// history keeps status only, not the browser's cached media or stream.
		source.snapshot = core.StreamStartupSnapshot{}
		source.stream, source.reader = nil, nil
		source.cancel, source.release = nil, nil
	})
}

func (s *sipLabSession) publishSourceLoop(audioPort, videoPort int) {
	err := s.forwardPublishSource(audioPort, videoPort)
	// Retire the sender before fail joins all media workers; a sender must
	// never wait for itself during transport cleanup.
	s.mediaWG.Done()
	if s.ctx.Err() == nil {
		s.gateway.labs.failSession(s, err)
	}
}

func (s *sipLabSession) forwardPublishSource(audioPort, videoPort int) error {
	source := s.source
	audio := mediarp.NewSession(labPayloadType(strings.TrimSpace(s.request.Codec)), 8000)
	video := mediarp.NewSession(96, 90000)
	audioPacketizer := &mediarp.G711Packetizer{}
	videoPacketizer := &mediarp.H264Packetizer{}
	header, headerErr := sipSourceVideoHeader(source.snapshot.VideoSequenceHeader)
	if headerErr != nil {
		return headerErr
	}
	waitingKeyframe := true
	var origin time.Time
	var originDTS int64
	send := func(frame *avframe.AVFrame) error {
		if err := source.active(); err != nil {
			return err
		}
		if frame == nil {
			return nil
		}
		if frame.MediaType.IsVideo() {
			if frame.Codec != avframe.CodecH264 {
				return fmt.Errorf("%w: SIP publish source video changed", ErrCodecMismatch)
			}
			if frame.FrameType == avframe.FrameTypeSequenceHeader {
				header, headerErr = sipSourceVideoHeader(frame)
				waitingKeyframe = true
				return headerErr
			}
			if waitingKeyframe && !frame.FrameType.IsKeyframe() {
				return nil
			}
		} else if frame.MediaType.IsAudio() {
			if frame.Codec != source.snapshot.MediaInfo.AudioCodec {
				return fmt.Errorf("%w: SIP publish source audio changed", ErrCodecMismatch)
			}
			if frame.FrameType == avframe.FrameTypeSequenceHeader {
				return nil
			}
		} else {
			return nil
		}
		if origin.IsZero() {
			origin, originDTS = time.Now(), frame.DTS
		}
		// Preserve source timestamp spacing while replaying the bounded GOP.
		// Rebase after a stall so delayed input cannot cause a catch-up burst.
		due := origin.Add(time.Duration(frame.DTS-originDTS) * time.Millisecond)
		if wait := time.Until(due); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-source.ctx.Done():
				timer.Stop()
				return errors.New("SIP publish source generation ended")
			case <-timer.C:
			}
		} else if wait < -100*time.Millisecond {
			origin, originDTS = time.Now(), frame.DTS
		}
		if frame.MediaType.IsVideo() {
			if frame.FrameType.IsKeyframe() {
				bootstrap := *header
				bootstrap.DTS, bootstrap.PTS = frame.DTS, frame.PTS
				if err := s.sendPublishSourceFrame(&bootstrap, videoPacketizer, video, s.videoRTPConn, s.videoRTCPConn, videoPort); err != nil {
					return err
				}
				waitingKeyframe = false
			}
			return s.sendPublishSourceFrame(frame, videoPacketizer, video, s.videoRTPConn, s.videoRTCPConn, videoPort)
		}
		return s.sendPublishSourceFrame(frame, audioPacketizer, audio, s.rtpConn, s.rtcpConn, audioPort)
	}
	for _, frame := range source.snapshot.ReplayFrames {
		if err := send(frame); err != nil {
			return err
		}
	}
	for {
		read := source.reader.ReadResultContext(source.ctx)
		if read.Overwritten != 0 {
			return fmt.Errorf("SIP publish source continuity lost: %d frames overwritten", read.Overwritten)
		}
		if !read.OK {
			return errors.New("SIP publish source ended")
		}
		if err := send(read.Value); err != nil {
			return err
		}
	}
}

func sipSourceVideoHeader(frame *avframe.AVFrame) (*avframe.AVFrame, error) {
	if frame == nil {
		return nil, fmt.Errorf("%w: SIP publish source has no H264 header", ErrCodecMismatch)
	}
	var payload []byte
	var hasSPS, hasPPS bool
	for _, nal := range h264.ExtractNALUs(mediarp.ToAnnexB(frame.Payload, true)) {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 31 {
		case 7:
			hasSPS = true
		case 8:
			hasPPS = true
		default:
			continue
		}
		payload = append(payload, 0, 0, 0, 1)
		payload = append(payload, nal...)
	}
	if !hasSPS || !hasPPS {
		return nil, fmt.Errorf("%w: SIP publish source requires H264 SPS/PPS", ErrCodecMismatch)
	}
	header := *frame
	header.Payload = payload
	return &header, nil
}

func (s *sipLabSession) sendPublishSourceFrame(frame *avframe.AVFrame, packetizer mediarp.Packetizer, session *mediarp.Session, conn, control *net.UDPConn, port int) error {
	packets, prepareErr := prepareSIPFrame(frame, packetizer, session)
	if prepareErr != nil {
		return prepareErr
	}
	remote := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}
	for _, packet := range packets {
		if err := s.source.active(); err != nil {
			return err
		}
		data, err := packet.Marshal()
		if err != nil {
			return err
		}
		n, err := conn.WriteToUDP(data, remote)
		if err != nil {
			return fmt.Errorf("send SIP publish source RTP: %w", err)
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
		s.rtpPacketsSent.Add(1)
		s.rtpBytesSent.Add(uint64(len(data)))
		if frame.MediaType.IsVideo() {
			s.videoRTPSent.Add(1)
		} else {
			s.audioRTPSent.Add(1)
		}
		s.markMedia()
	}
	if err := s.source.active(); err != nil {
		return err
	}
	data, err := (&rtcp.ReceiverReport{SSRC: session.SSRC}).Marshal()
	if err != nil {
		return err
	}
	remote.Port++
	if _, err := control.WriteToUDP(data, remote); err != nil {
		return fmt.Errorf("send SIP publish source RTCP: %w", err)
	}
	s.rtcpPacketsSent.Add(1)
	return nil
}
