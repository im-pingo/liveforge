package gb28181

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/h264"
	"github.com/im-pingo/liveforge/pkg/muxer/ps"
	"github.com/im-pingo/liveforge/pkg/util"
)

const gbLabSourceStartupTimeout = 5 * time.Second

type gbLabPublishSource struct {
	stream     *core.Stream
	snapshot   core.StreamStartupSnapshot
	reader     *util.RingReader[*avframe.AVFrame]
	parameters []byte
	release    func()
	closeOnce  sync.Once
}

// Admit one captured generation before allocating fake-device signaling or
// media sockets. Unknown tracks and late headers are bounded and cancellable.
func (s *gbLabSession) preparePublishSource(parent context.Context) (*gbLabPublishSource, error) {
	stream, ok := s.module.handler.hub.Find(s.request.SourceStreamKey)
	if !ok {
		return nil, fmt.Errorf("%w: publish source stream does not exist", ErrLabInvalidRequest)
	}
	pending := stream.StartupSnapshot()
	if pending.Generation == 0 || pending.GenerationDone == nil || pending.PublisherID == "" || !stream.IsPublisherGeneration(pending.Generation) {
		return nil, fmt.Errorf("%w: publish source has no active publisher", ErrLabInvalidRequest)
	}
	ctx, cancel := context.WithTimeout(parent, gbLabSourceStartupTimeout)
	defer cancel()
	bound, cancelGeneration := bindGBGeneration(ctx, pending)
	defer cancelGeneration()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	snapshot := pending
	for {
		if err := bound.Err(); err != nil {
			return nil, fmt.Errorf("GB28181 publish source startup: %w", err)
		}
		if snapshot.Generation != pending.Generation || !stream.IsPublisherGeneration(pending.Generation) {
			return nil, errors.New("GB28181 publish source generation ended")
		}
		info := snapshot.MediaInfo
		if (info.VideoCodec != 0 && info.VideoCodec != avframe.CodecH264) || (info.AudioCodec != 0 && info.AudioCodec != avframe.CodecG711A) || (info.SampleRate != 0 && info.SampleRate != 8000) || (info.Channels != 0 && info.Channels != 1) {
			return nil, fmt.Errorf("%w: publish source requires H.264 and 8 kHz mono G.711A", ErrLabInvalidRequest)
		}
		if snapshot.Ready && info.VideoCodec == avframe.CodecH264 && info.AudioCodec == avframe.CodecG711A && snapshot.VideoSequenceHeader != nil {
			break
		}
		select {
		case <-bound.Done():
			return nil, fmt.Errorf("GB28181 publish source startup: %w", bound.Err())
		case <-ticker.C:
			snapshot = stream.StartupSnapshot()
		}
	}
	parameters, err := gbLabSourceParameters(snapshot.VideoSequenceHeader.Payload)
	if err != nil {
		return nil, fmt.Errorf("%w: source H.264 header: %w", ErrLabInvalidRequest, err)
	}
	release, err := stream.AddSubscriberForGeneration("gb28181", snapshot.Generation)
	if err != nil {
		return nil, err
	}
	return &gbLabPublishSource{stream: stream, snapshot: snapshot, reader: stream.RingBuffer().NewReaderAt(snapshot.LiveCursor), parameters: parameters, release: release}, nil
}

func (s *gbLabPublishSource) close() {
	s.closeOnce.Do(func() { s.reader.Close(); s.release() })
}

// discardReferences runs only after the session has joined every media worker.
// Terminal Lab history needs counters and request identity, not captured media.
func (s *gbLabPublishSource) discardReferences() {
	s.close()
	s.stream, s.reader, s.release = nil, nil, nil
	s.snapshot = core.StreamStartupSnapshot{}
	s.parameters = nil
}

func (s *gbLabPublishSource) active(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.snapshot.GenerationDone:
		return errors.New("GB28181 publish source generation ended")
	default:
	}
	if !s.stream.IsPublisherGeneration(s.snapshot.Generation) {
		return errors.New("GB28181 publish source generation ended")
	}
	return nil
}

func (s *gbLabSession) publishSourceMediaLoop(remotePort int) error {
	source := s.source
	defer source.close()
	readCtx, cancel := bindGBGeneration(s.ctx, source.snapshot)
	defer cancel()
	remoteRTP := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: remotePort}
	remoteRTCP := &net.UDPAddr{IP: remoteRTP.IP, Port: remotePort + 1}
	muxer := ps.NewMuxer()
	waitingKeyframe := true
	send := func(frame *avframe.AVFrame) error {
		if err := source.active(s.ctx); err != nil {
			return err
		}
		if frame == nil {
			return nil
		}
		if frame.MediaType.IsAudio() {
			if frame.Codec != avframe.CodecG711A {
				return errors.New("GB28181 publish source audio codec changed")
			}
			if frame.FrameType == avframe.FrameTypeSequenceHeader {
				return nil
			}
			return s.sendFrame(remoteRTP, remoteRTCP, muxer, frame)
		}
		if !frame.MediaType.IsVideo() {
			return nil
		}
		if frame.Codec != avframe.CodecH264 {
			return errors.New("GB28181 publish source video codec changed")
		}
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			parameters, err := gbLabSourceParameters(frame.Payload)
			if err != nil {
				return err
			}
			source.parameters = parameters
			return nil
		}
		if waitingKeyframe && !frame.FrameType.IsKeyframe() {
			return nil
		}
		payload, err := gbLabSourceAnnexB(frame.Payload)
		if err != nil {
			return err
		}
		if frame.FrameType.IsKeyframe() {
			// PS ingress derives decoder configuration from the keyframe access unit.
			// WHIP keeps configuration separately, so restore SPS/PPS beside the IDR.
			payload = append(append([]byte(nil), source.parameters...), payload...)
			waitingKeyframe = false
		}
		copy := *frame
		copy.Payload = payload
		return s.sendFrame(remoteRTP, remoteRTCP, muxer, &copy)
	}
	for _, frame := range source.snapshot.ReplayFrames {
		if err := send(frame); err != nil {
			return err
		}
	}
	for {
		result := source.reader.ReadResultContext(readCtx)
		if err := source.active(s.ctx); err != nil {
			return err
		}
		if result.Overwritten > 0 {
			return fmt.Errorf("GB28181 publish source overwritten by %d frames", result.Overwritten)
		}
		if !result.OK {
			return errors.New("GB28181 publish source reader ended")
		}
		if err := send(result.Value); err != nil {
			return err
		}
	}
}

func gbLabSourceParameters(payload []byte) ([]byte, error) {
	var sps, pps []byte
	if len(payload) > 0 && payload[0] == 1 {
		var err error
		sps, pps, err = h264.ExtractSPSPPSFromAVCRecord(payload)
		if err != nil {
			return nil, err
		}
	} else {
		for _, nal := range h264.ExtractNALUs(payload) {
			if len(nal) > 0 {
				switch nal[0] & 31 {
				case 7:
					sps = nal
				case 8:
					pps = nal
				}
			}
		}
	}
	if len(sps) == 0 || len(pps) == 0 || sps[0]&31 != 7 || pps[0]&31 != 8 {
		return nil, errors.New("missing H.264 SPS/PPS")
	}
	result := append([]byte{0, 0, 0, 1}, sps...)
	result = append(result, 0, 0, 0, 1)
	return append(result, pps...), nil
}

func gbLabSourceAnnexB(payload []byte) ([]byte, error) {
	if len(payload) >= 3 && payload[0] == 0 && payload[1] == 0 && (payload[2] == 1 || (len(payload) >= 4 && payload[2] == 0 && payload[3] == 1)) {
		return payload, nil
	}
	// Validate every AVCC length before converting; partial access units are not
	// playable and must not be silently forwarded as a shorter frame.
	remaining := payload
	for len(remaining) >= 4 {
		size := uint64(binary.BigEndian.Uint32(remaining))
		remaining = remaining[4:]
		if size == 0 || size > uint64(len(remaining)) {
			return nil, errors.New("invalid source H.264 AVCC frame")
		}
		remaining = remaining[size:]
	}
	if len(payload) == 0 || len(remaining) != 0 {
		return nil, errors.New("invalid source H.264 AVCC frame")
	}
	return h264.AVCCToAnnexB(payload), nil
}
