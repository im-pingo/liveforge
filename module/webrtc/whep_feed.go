package webrtc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/h265"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/util"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// WHEPFeedState describes the part of the playback startup lifecycle that is
// useful to a browser and to an operator diagnosing a stalled session.
type WHEPFeedState string

const (
	WHEPFeedWaitingKeyframe   WHEPFeedState = "waiting_keyframe"
	WHEPFeedPlaying           WHEPFeedState = "playing"
	WHEPFeedNoMediaInput      WHEPFeedState = "no_media_input"
	WHEPFeedMediaStalled      WHEPFeedState = "media_stalled"
	WHEPFeedCodecMismatch     WHEPFeedState = "codec_mismatch"
	WHEPFeedSampleWriteFailed WHEPFeedState = "sample_write_failed"
	WHEPFeedTargetAudioFailed WHEPFeedState = "target_audio_failed"
	WHEPFeedGenerationEnded   WHEPFeedState = "generation_ended"
	WHEPFeedClosed            WHEPFeedState = "closed"
)

const whepNoMediaInputTimeout = 8 * time.Second

// WHEPFeedStatus is a point-in-time diagnostic snapshot. Counters are
// intentionally session-local so one stalled subscriber can be diagnosed
// without turning stream metrics into high-cardinality labels.
type WHEPFeedStatus struct {
	Generation          uint64        `json:"generation"`
	Cursor              int64         `json:"cursor"`
	Mode                string        `json:"mode"`
	State               WHEPFeedState `json:"state"`
	FirstMediaAt        time.Time     `json:"first_media_at,omitempty"`
	FirstMediaWaitMS    int64         `json:"first_media_wait_ms"`
	LastVideoAt         time.Time     `json:"last_video_at,omitempty"`
	LastAudioAt         time.Time     `json:"last_audio_at,omitempty"`
	UpdatedAt           time.Time     `json:"updated_at"`
	ExpectedVideo       bool          `json:"expected_video"`
	ExpectedAudio       bool          `json:"expected_audio"`
	VideoFrames         uint64        `json:"video_frames"`
	AudioFrames         uint64        `json:"audio_frames"`
	DroppedVideo        uint64        `json:"dropped_video"`
	DroppedAudio        uint64        `json:"dropped_audio"`
	SourceOverwrites    uint64        `json:"source_overwrites"`
	RTPPacketsSent      uint64        `json:"rtp_packets_sent"`
	RTPBytesSent        uint64        `json:"rtp_bytes_sent"`
	RTCPPacketsReceived uint64        `json:"rtcp_packets_received"`
	LastError           string        `json:"last_error,omitempty"`
}

type whepFeedPhase struct {
	state     WHEPFeedState
	changedAt int64
}

type whepFeedStatus struct {
	generation uint64
	cursor     int64
	mode       string

	phase            atomic.Pointer[whepFeedPhase]
	updateMu         sync.Mutex
	terminal         atomic.Bool
	createdAt        atomic.Int64
	firstMediaAt     atomic.Int64
	lastVideoAt      atomic.Int64
	lastAudioAt      atomic.Int64
	updatedAt        atomic.Int64
	expectedVideo    atomic.Bool
	expectedAudio    atomic.Bool
	videoFrames      atomic.Uint64
	audioFrames      atomic.Uint64
	droppedVideo     atomic.Uint64
	droppedAudio     atomic.Uint64
	sourceOverwrites atomic.Uint64
	rtpPackets       atomic.Uint64
	rtpBytes         atomic.Uint64
	rtcpPackets      atomic.Uint64

	errorMu   sync.RWMutex
	lastError string
}

func newWHEPFeedStatus(generation uint64, cursor int64, mode string) *whepFeedStatus {
	now := time.Now().UTC()
	status := &whepFeedStatus{generation: generation, cursor: cursor, mode: mode}
	status.phase.Store(&whepFeedPhase{state: WHEPFeedWaitingKeyframe, changedAt: now.UnixNano()})
	status.createdAt.Store(now.UnixNano())
	status.updatedAt.Store(now.UnixNano())
	return status
}

func (s *whepFeedStatus) Snapshot() WHEPFeedStatus {
	phase := s.phase.Load()
	createdAt := s.createdAt.Load()
	firstMediaAt := s.firstMediaAt.Load()
	firstMediaWaitMS := int64(0)
	if createdAt > 0 && firstMediaAt > createdAt {
		firstMediaWaitMS = (firstMediaAt - createdAt) / int64(time.Millisecond)
	}
	s.errorMu.RLock()
	lastError := s.lastError
	s.errorMu.RUnlock()
	return WHEPFeedStatus{
		Generation:          s.generation,
		Cursor:              s.cursor,
		Mode:                s.mode,
		State:               phase.state,
		FirstMediaAt:        whepTimeFromUnixNano(firstMediaAt),
		FirstMediaWaitMS:    firstMediaWaitMS,
		LastVideoAt:         whepTimeFromUnixNano(s.lastVideoAt.Load()),
		LastAudioAt:         whepTimeFromUnixNano(s.lastAudioAt.Load()),
		UpdatedAt:           whepTimeFromUnixNano(s.updatedAt.Load()),
		ExpectedVideo:       s.expectedVideo.Load(),
		ExpectedAudio:       s.expectedAudio.Load(),
		VideoFrames:         s.videoFrames.Load(),
		AudioFrames:         s.audioFrames.Load(),
		DroppedVideo:        s.droppedVideo.Load(),
		DroppedAudio:        s.droppedAudio.Load(),
		SourceOverwrites:    s.sourceOverwrites.Load(),
		RTPPacketsSent:      s.rtpPackets.Load(),
		RTPBytesSent:        s.rtpBytes.Load(),
		RTCPPacketsReceived: s.rtcpPackets.Load(),
		LastError:           lastError,
	}
}

func (s *whepFeedStatus) SetState(state WHEPFeedState) {
	now := time.Now().UTC()
	if isWHEPFeedTerminal(state) {
		s.setTerminalAt(state, nil, now)
		return
	}
	if !s.beginUpdate() {
		return
	}
	defer s.endUpdate()
	s.setNonterminalStateAt(state, now)
	s.updatedAt.Store(now.UnixNano())
}

func (s *whepFeedStatus) SetError(state WHEPFeedState, err error) {
	s.setTerminalAt(state, err, time.Now().UTC())
}

func (s *whepFeedStatus) RecordVideo(sent bool) {
	s.recordVideoAt(sent, time.Now().UTC())
}

func (s *whepFeedStatus) recordVideoAt(sent bool, now time.Time) {
	if !s.beginUpdate() {
		return
	}
	defer s.endUpdate()
	nowUnix := now.UnixNano()
	if sent {
		s.videoFrames.Add(1)
		s.lastVideoAt.Store(nowUnix)
		s.firstMediaAt.CompareAndSwap(0, nowUnix)
		s.updatePlayingAt(now)
	} else {
		s.droppedVideo.Add(1)
		if phase := s.phase.Load(); phase.state == WHEPFeedNoMediaInput && s.lastVideoAt.Load() == 0 {
			s.setNonterminalStateAt(WHEPFeedWaitingKeyframe, now)
		}
	}
	s.updatedAt.Store(nowUnix)
}

func (s *whepFeedStatus) RecordAudio(sent bool) {
	s.recordAudioAt(sent, time.Now().UTC())
}

func (s *whepFeedStatus) beginVideoRecovery() {
	if !s.beginUpdate() {
		return
	}
	defer s.endUpdate()
	now := time.Now().UTC()
	s.lastVideoAt.Store(0)
	s.setNonterminalStateAt(WHEPFeedWaitingKeyframe, now)
	s.updatedAt.Store(now.UnixNano())
}

func (s *whepFeedStatus) recordSourceOverwrite(dropped uint64) {
	if dropped == 0 || !s.beginUpdate() {
		return
	}
	defer s.endUpdate()
	s.sourceOverwrites.Add(dropped)
	s.updatedAt.Store(time.Now().UTC().UnixNano())
}

func (s *whepFeedStatus) recordDroppedAudio(dropped uint64) {
	if dropped == 0 || !s.beginUpdate() {
		return
	}
	defer s.endUpdate()
	s.droppedAudio.Add(dropped)
	s.updatedAt.Store(time.Now().UTC().UnixNano())
}

func (s *whepFeedStatus) recordAudioAt(sent bool, now time.Time) {
	if !s.beginUpdate() {
		return
	}
	defer s.endUpdate()
	nowUnix := now.UnixNano()
	if sent {
		s.audioFrames.Add(1)
		s.lastAudioAt.Store(nowUnix)
		s.firstMediaAt.CompareAndSwap(0, nowUnix)
		s.updatePlayingAt(now)
	} else {
		s.droppedAudio.Add(1)
	}
	s.updatedAt.Store(nowUnix)
}

func (s *whepFeedStatus) MarkNoMediaInput() {
	now := time.Now().UTC()
	if s.videoFrames.Load()+s.audioFrames.Load()+s.droppedVideo.Load()+s.droppedAudio.Load() == 0 {
		if !s.beginUpdate() {
			return
		}
		defer s.endUpdate()
		s.setNonterminalStateAt(WHEPFeedNoMediaInput, now)
		s.updatedAt.Store(now.UnixNano())
		return
	}
	s.checkInactivityAt(now)
}

func (s *whepFeedStatus) checkInactivityAt(now time.Time) {
	if !s.beginUpdate() {
		return
	}
	defer s.endUpdate()
	if s.expectedVideo.Load() && s.lastVideoAt.Load() == 0 && s.droppedVideo.Load() > 0 {
		s.setNonterminalStateAt(WHEPFeedWaitingKeyframe, now)
		return
	}
	if s.videoFrames.Load()+s.audioFrames.Load() == 0 {
		if now.UnixNano()-s.createdAt.Load() >= whepNoMediaInputTimeout.Nanoseconds() {
			s.setNonterminalStateAt(WHEPFeedNoMediaInput, now)
			s.updatedAt.Store(now.UnixNano())
		}
		return
	}
	if s.missingExpectedMediaWithinStartupGraceAt(now) {
		return
	}
	if s.mediaReadyAt(now) {
		s.setNonterminalStateAt(WHEPFeedPlaying, now)
		return
	}
	s.setNonterminalStateAt(WHEPFeedMediaStalled, now)
	s.updatedAt.Store(now.UnixNano())
}

func (s *whepFeedStatus) missingExpectedMediaWithinStartupGraceAt(now time.Time) bool {
	firstMediaAt := s.firstMediaAt.Load()
	if firstMediaAt == 0 || now.UnixNano()-firstMediaAt >= whepNoMediaInputTimeout.Nanoseconds() {
		return false
	}
	return s.expectedVideo.Load() && s.lastVideoAt.Load() == 0 ||
		s.expectedAudio.Load() && s.lastAudioAt.Load() == 0
}

func (s *whepFeedStatus) SetTransportStats(rtpPackets, rtpBytes, rtcpPackets uint64) {
	if !s.beginUpdate() {
		return
	}
	defer s.endUpdate()
	changed := storeAtomicMaximum(&s.rtpPackets, rtpPackets)
	changed = storeAtomicMaximum(&s.rtpBytes, rtpBytes) || changed
	changed = storeAtomicMaximum(&s.rtcpPackets, rtcpPackets) || changed
	if changed {
		s.updatedAt.Store(time.Now().UTC().UnixNano())
	}
}

func (s *whepFeedStatus) setFinalTransportStats(rtpPackets, rtpBytes, rtcpPackets uint64) {
	storeAtomicMaximum(&s.rtpPackets, rtpPackets)
	storeAtomicMaximum(&s.rtpBytes, rtpBytes)
	storeAtomicMaximum(&s.rtcpPackets, rtcpPackets)
}

func (s *whepFeedStatus) setExpectedMedia(video, audio bool) {
	s.expectedVideo.Store(video)
	s.expectedAudio.Store(audio)
}

func (s *whepFeedStatus) watchInactivity(stop, generationDone <-chan struct{}, timeout time.Duration) {
	if timeout <= 0 {
		timeout = whepNoMediaInputTimeout
	}
	interval := timeout / 4
	if interval <= 0 {
		interval = timeout
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			s.checkInactivityAt(now.UTC())
		case <-stop:
			return
		case <-generationDone:
			return
		}
	}
}

func (s *whepFeedStatus) updatePlayingAt(now time.Time) {
	if s.mediaReadyAt(now) {
		s.setNonterminalStateAt(WHEPFeedPlaying, now)
	}
}

func (s *whepFeedStatus) mediaReadyAt(now time.Time) bool {
	expectedVideo := s.expectedVideo.Load()
	expectedAudio := s.expectedAudio.Load()
	if !expectedVideo && !expectedAudio {
		return s.videoFrames.Load()+s.audioFrames.Load() > 0
	}
	deadline := now.UnixNano() - whepNoMediaInputTimeout.Nanoseconds()
	if expectedVideo {
		last := s.lastVideoAt.Load()
		if last == 0 || last <= deadline {
			return false
		}
	}
	if expectedAudio {
		last := s.lastAudioAt.Load()
		if last == 0 || last <= deadline {
			return false
		}
	}
	return true
}

func (s *whepFeedStatus) setNonterminalStateAt(state WHEPFeedState, now time.Time) {
	nowUnix := now.UnixNano()
	for {
		current := s.phase.Load()
		if isWHEPFeedTerminal(current.state) || nowUnix < current.changedAt || current.state == state {
			return
		}
		next := &whepFeedPhase{state: state, changedAt: nowUnix}
		if s.phase.CompareAndSwap(current, next) {
			s.logStateTransition(current.state, state, "")
			return
		}
	}
}

func (s *whepFeedStatus) setTerminalAt(state WHEPFeedState, err error, now time.Time) {
	if !s.closeUpdates() {
		return
	}
	defer s.updateMu.Unlock()
	previous := s.phase.Load().state
	lastError := ""
	if err != nil {
		lastError = boundedWHEPError(err)
		s.errorMu.Lock()
		s.lastError = lastError
		s.errorMu.Unlock()
	}
	nowUnix := now.UnixNano()
	s.phase.Store(&whepFeedPhase{state: state, changedAt: nowUnix})
	s.updatedAt.Store(nowUnix)
	s.logStateTransition(previous, state, lastError)
}

func (s *whepFeedStatus) logStateTransition(previous, state WHEPFeedState, lastError string) {
	attributes := []any{
		"module", "webrtc",
		"generation", s.generation,
		"cursor", s.cursor,
		"mode", s.mode,
		"previous", previous,
		"state", state,
	}
	if lastError != "" {
		attributes = append(attributes, "error", lastError)
	}
	slog.Info("WHEP feed state changed", attributes...)
}

func (s *whepFeedStatus) beginUpdate() bool {
	s.updateMu.Lock()
	if s.terminal.Load() {
		s.updateMu.Unlock()
		return false
	}
	return true
}

func (s *whepFeedStatus) endUpdate() {
	s.updateMu.Unlock()
}

func (s *whepFeedStatus) closeUpdates() bool {
	s.updateMu.Lock()
	if s.terminal.Load() {
		s.updateMu.Unlock()
		return false
	}
	s.terminal.Store(true)
	return true
}

func storeAtomicMaximum(destination *atomic.Uint64, value uint64) bool {
	for {
		current := destination.Load()
		if value <= current {
			return false
		}
		if destination.CompareAndSwap(current, value) {
			return true
		}
	}
}

func whepTimeFromUnixNano(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

func isWHEPFeedTerminal(state WHEPFeedState) bool {
	switch state {
	case WHEPFeedCodecMismatch, WHEPFeedSampleWriteFailed,
		WHEPFeedTargetAudioFailed, WHEPFeedGenerationEnded, WHEPFeedClosed:
		return true
	default:
		return false
	}
}

func boundedWHEPError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if len(message) > 512 {
		return message[:512]
	}
	return message
}

// whepFeedLoop reads AVFrames from the stream's RingBuffer and writes them
// to the WebRTC tracks via TrackSender. It waits for the peer connection to
// be established before sending any data.
//
// PLI/FIR handling is NOT done here — it runs independently in each
// TrackSender's RTCP goroutine, decoupled from this feed goroutine.
//
// mode controls startup behavior:
//   - "realtime": skip GOP cache, read live frames, discard until first keyframe.
//   - "live": send GOP cache (paced at 10x speed), then live frames.
func whepFeedLoop(stream *core.Stream, startup core.StreamStartupSnapshot, video, audio *TrackSender, done <-chan struct{}, connected <-chan struct{}, mode string, targetAudioCodec avframe.CodecType, bwe cc.BandwidthEstimator, status *whepFeedStatus, sendGates ...*whepSendGate) {
	var sendGate *whepSendGate
	if len(sendGates) > 0 {
		sendGate = sendGates[0]
	}
	if sendGate == nil {
		sendGate = newWHEPSendGate()
	}
	if status == nil {
		status = newWHEPFeedStatus(startup.Generation, startup.LiveCursor, mode)
	}
	defer func() {
		if !isWHEPFeedTerminal(status.Snapshot().State) {
			select {
			case <-startup.GenerationDone:
				status.SetState(WHEPFeedGenerationEnded)
			default:
				status.SetState(WHEPFeedClosed)
			}
		}
	}()
	gateStop := make(chan struct{})
	gateDone := make(chan struct{})
	go func() {
		defer close(gateDone)
		select {
		case <-startup.GenerationDone:
		case <-done:
		case <-gateStop:
			return
		}
		sendGate.close()
	}()
	defer func() {
		sendGate.close()
		close(gateStop)
		<-gateDone
	}()

	// Wait for ICE+DTLS to complete before sending media.
	select {
	case <-connected:
		slog.Info("peer connected, starting media feed", "module", "webrtc", "mode", mode)
	case <-done:
		return
	case <-startup.GenerationDone:
		return
	}
	watchdogStop := make(chan struct{})
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		status.watchInactivity(watchdogStop, startup.GenerationDone, whepNoMediaInputTimeout)
	}()
	defer func() {
		close(watchdogStop)
		<-watchdogDone
	}()

	if bwe != nil {
		bwe.OnTargetBitrateChange(func(bitrate int) {
			slog.Debug("GCC target bitrate changed",
				"module", "webrtc",
				"bitrate_kbps", bitrate/1000,
			)
		})
	}

	// Determine if audio transcoding is needed.
	sourceAudioCodec := startup.MediaInfo.AudioCodec
	needsTranscode := targetAudioCodec != sourceAudioCodec && sourceAudioCodec != 0
	videoCodec := startup.MediaInfo.VideoCodec

	// Track the last DTS to compute sample durations.
	var lastVideoDTS, lastAudioDTS int64

	// For H264/H265: cache parameter sets (SPS/PPS or VPS/SPS/PPS) in Annex-B
	// format to prepend to keyframes. Chromium requires parameter sets to share
	// the same RTP timestamp as the IDR NAL.
	var paramSetBuf []byte
	needsAnnexB := videoCodec == avframe.CodecH264 || videoCodec == avframe.CodecH265
	if needsAnnexB {
		if sh := startup.VideoSequenceHeader; sh != nil {
			if !whepParameterSetsReady(videoCodec, sh.Payload) {
				status.SetError(WHEPFeedCodecMismatch, errors.New("invalid video sequence header: required parameter sets are missing"))
				return
			}
			paramSetBuf = pkgrtp.VideoToAnnexB(videoCodec, sh.Payload, true)
		}
	}
	refreshVideoParameterSets := func() bool {
		if !needsAnnexB {
			return true
		}
		current := stream.StartupSnapshot()
		if current.StreamInstanceID != startup.StreamInstanceID || current.Generation != startup.Generation {
			return false
		}
		if sh := current.VideoSequenceHeader; sh != nil {
			if !whepParameterSetsReady(videoCodec, sh.Payload) {
				status.SetError(WHEPFeedCodecMismatch, errors.New("invalid video sequence header: required parameter sets are missing"))
				return false
			}
			paramSetBuf = pkgrtp.VideoToAnnexB(videoCodec, sh.Payload, true)
		}
		return true
	}

	// B-frame drop: Chrome's WebRTC H.264 decoder does not perform B-frame
	// reordering (it's designed for Baseline profile). Sending B-frames
	// causes either visual jitter (DTS-order) or mosaic corruption
	// (PTS-order). We drop B-frames and send only I/P reference frames.
	//
	// Detection: for H.264, track the highest PTS sent. Any frame whose PTS is below
	// that threshold is a B-frame (its display time precedes a previously
	// decoded reference frame). When there are no B-frames (PTS == DTS),
	// all frames pass through since PTS is always increasing.
	var maxSentVideoPTS int64
	var lastSentVideoDTS int64 = -1

	// writeVideoSample writes one video frame to the WebRTC track.
	//
	// Codec-specific handling:
	//   H264/H265: AVCC/HVCC → Annex-B conversion; parameter sets prepended
	//              to keyframes so the decoder sees them in the same access unit.
	//   VP8/VP9/AV1: raw frame data passed directly to pion's packetizer.
	//
	// PLI/FIR resync: inter-frames are skipped until the next keyframe.
	// H.264 B-frame drop: frames with PTS < maxSentVideoPTS are silently dropped.
	writeVideoSample := func(frame *avframe.AVFrame) bool {
		if video == nil {
			return false
		}

		// SequenceHeader: cache parameter sets, do not send as a sample.
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			if needsAnnexB {
				if !whepParameterSetsReady(videoCodec, frame.Payload) {
					status.SetError(WHEPFeedCodecMismatch, errors.New("invalid video sequence header: required parameter sets are missing"))
					return false
				}
				paramSetBuf = pkgrtp.VideoToAnnexB(videoCodec, frame.Payload, true)
			}
			return false
		}

		// PLI/FIR resync: skip inter-frames until the next keyframe.
		// Reset DTS tracker so the first keyframe after resync
		// gets a normal ~40ms duration instead of a multi-second gap.
		if video.NeedsKeyframe() {
			if frame.FrameType != avframe.FrameTypeKeyframe {
				status.RecordVideo(false)
				if frame.DTS > 0 {
					lastVideoDTS = frame.DTS
				}
				lastSentVideoDTS = -1 // reset so keyframe gets default duration
				return false
			}
			video.ClearNeedsKeyframe()
			slog.Debug("PLI resync: sending keyframe", "module", "webrtc", "bytes", len(frame.Payload))
		}

		// Drop H.264 B-frames whose display time precedes an already-sent
		// reference frame. H.265 B-frames may themselves be references and
		// must stay in the decode-order stream.
		if shouldDropWHEPVideoFrame(videoCodec, frame, maxSentVideoPTS) {
			status.RecordVideo(false)
			if frame.DTS > 0 {
				lastVideoDTS = frame.DTS
			}
			return false
		}

		var payload []byte
		if needsAnnexB {
			// H264/H265: convert AVCC/HVCC length-prefixed NALs to Annex-B.
			payload = pkgrtp.VideoToAnnexB(videoCodec, frame.Payload, false)
			if len(payload) == 0 {
				status.SetError(WHEPFeedCodecMismatch, errors.New("empty video access unit"))
				return false
			}
			// Prepend parameter sets to keyframes.
			if frame.FrameType == avframe.FrameTypeKeyframe && len(paramSetBuf) > 0 {
				combined := make([]byte, len(paramSetBuf)+len(payload))
				copy(combined, paramSetBuf)
				copy(combined[len(paramSetBuf):], payload)
				payload = combined
			}
		} else {
			// VP8/VP9/AV1: raw frame data, no conversion needed.
			payload = frame.Payload
			if len(payload) == 0 {
				status.RecordVideo(false)
				return false
			}
		}

		// Track DTS for the pacer (used in live mode GOP→live transition).
		if frame.DTS > 0 {
			lastVideoDTS = frame.DTS
		}

		// Compute duration from DTS delta between sent frames.
		// This drives RTP timestamp advancement in pion's packetizer.
		//
		// When B-frames are dropped, lastSentVideoDTS tracks the DTS of
		// the previous *sent* frame, so the duration correctly spans the
		// gap including dropped B-frames (matching the DTS pacer's delivery).
		duration := time.Duration(0)
		if lastSentVideoDTS >= 0 && frame.DTS > lastSentVideoDTS {
			duration = time.Duration(frame.DTS-lastSentVideoDTS) * time.Millisecond
		} else {
			duration = 40 * time.Millisecond // ~25fps default
		}
		lastSentVideoDTS = frame.DTS

		if frame.PTS > maxSentVideoPTS {
			maxSentVideoPTS = frame.PTS
		}

		if err := writeWHEPSample(sendGate, video, media.Sample{
			Data:     payload,
			Duration: duration,
		}); err != nil {
			status.SetError(WHEPFeedSampleWriteFailed, err)
			slog.Warn("whep: video sample write failed", "module", "webrtc", "generation", startup.Generation, "error", err)
			return false
		}
		status.RecordVideo(true)
		return true
	}

	// Compute fixed audio frame duration for transcoded Opus.
	// Using a fixed duration avoids any DTS-delta precision issues and
	// ensures RTP timestamps advance by exactly 960 ticks per frame
	// (20ms × 48kHz), matching the actual Opus content duration.
	var fixedAudioDur time.Duration
	if needsTranscode && targetAudioCodec == avframe.CodecOpus {
		fixedAudioDur = 20 * time.Millisecond // 960 samples / 48kHz
	}

	// writeAudioSample writes only frames matching the negotiated track codec.
	// This prevents source AAC from being packetized on an Opus track if a
	// transcoder fails or a source-codec cache reaches this path.
	writeAudioSample := func(frame *avframe.AVFrame) bool {
		if audio == nil {
			return true
		}

		if !whepAudioFrameAllowed(frame, targetAudioCodec) {
			status.RecordAudio(false)
			return true
		}
		payload := frame.Payload

		var duration time.Duration
		if fixedAudioDur > 0 {
			// Transcoded Opus: use fixed duration for exact RTP timestamp spacing.
			duration = fixedAudioDur
		} else if lastAudioDTS > 0 && frame.DTS > lastAudioDTS {
			// Direct passthrough: compute from DTS delta.
			duration = time.Duration(frame.DTS-lastAudioDTS) * time.Millisecond
		} else {
			duration = 20 * time.Millisecond // safe default for most codecs
		}
		if frame.DTS > 0 {
			lastAudioDTS = frame.DTS
		}

		if err := writeWHEPSample(sendGate, audio, media.Sample{
			Data:     payload,
			Duration: duration,
		}); err != nil {
			status.SetError(WHEPFeedSampleWriteFailed, err)
			slog.Warn("whep: audio sample write failed", "module", "webrtc", "generation", startup.Generation, "error", err)
			return false
		}
		status.RecordAudio(true)
		return true
	}

	var gopCache []*avframe.AVFrame
	if mode == "live" {
		gopCache = whepLiveSnapshot(startup, needsTranscode)
	}

	readers, readerErr := newWHEPFeedReaders(stream, startup, needsTranscode, targetAudioCodec)
	if readerErr != nil {
		status.SetError(WHEPFeedTargetAudioFailed, readerErr)
		return
	}
	defer readers.Close()

	// Live mode: send the cached GOP so the subscriber gets an immediate
	// keyframe, paced at 10x real-time speed. When transcoding, the snapshot
	// contains video only; source-codec audio must never enter the target track.
	cacheKeyframeSent := false
	if mode == "live" {
		var prevDTS int64
		for _, frame := range gopCache {
			if !stream.IsPublisherGeneration(startup.Generation) {
				return
			}
			if frame.MediaType.IsVideo() {
				if writeVideoSample(frame) && frame.FrameType == avframe.FrameTypeKeyframe {
					cacheKeyframeSent = true
				}
			} else if frame.MediaType.IsAudio() {
				if !writeAudioSample(frame) {
					return
				}
			}
			if isWHEPFeedTerminal(status.Snapshot().State) {
				return
			}
			if frame.DTS > 0 && prevDTS > 0 {
				dtMs := frame.DTS - prevDTS
				if dtMs > 0 {
					sleep := time.Duration(dtMs) * time.Millisecond / 10
					if sleep > 0 && sleep < 50*time.Millisecond {
						timer := time.NewTimer(sleep)
						select {
						case <-timer.C:
						case <-done:
							timer.Stop()
							return
						case <-startup.GenerationDone:
							timer.Stop()
							return
						}
					}
				}
			}
			if frame.DTS > 0 {
				prevDTS = frame.DTS
			}
		}
		// Preserve last DTS from GOP cache so the first live frame gets a
		// proper duration delta instead of falling back to the default.
		// (lastVideoDTS / lastAudioDTS are already set by writeVideoSample /
		// writeAudioSample during cache playback.)
	}

	// DTS-based pacer: track wall-clock reference point to prevent bursting.
	// pion's WriteSample sends RTP packets immediately (no internal pacing).
	// Without pacing, the feed loop sends all buffered frames in a burst
	// whenever the ring buffer signals, causing extreme jitter (437ms stdev)
	// and browser jitter buffer inflation (3+ seconds).
	//
	// In live mode, initialize the pacer base NOW so that frames which
	// accumulated in the ring buffer during GOP cache playback are paced
	// from this moment rather than burst-sent. This bridges the GOP→live
	// transition smoothly and prevents initial freezes.
	var paceBaseWall time.Time
	var paceBaseDTS int64
	if mode == "live" && lastVideoDTS > 0 {
		paceBaseWall = time.Now()
		paceBaseDTS = lastVideoDTS
	}

	// Live frame loop: start reading only NEW frames (after snapshot).
	// In realtime mode, skip all frames until the first video keyframe
	// arrives, then start sending from that keyframe onward.
	gotKeyframe := whepInitialMediaReady(mode, cacheKeyframeSent, video != nil)
	readers.startWaiters(done, startup.GenerationDone)
	for {
		select {
		case <-done:
			return
		case <-startup.GenerationDone:
			return
		default:
		}
		if readers.activeTargetAudioEOF(done, startup.GenerationDone) {
			status.SetError(WHEPFeedTargetAudioFailed, errors.New("target audio reader closed during active WHEP feed"))
			return
		}
		if !readers.drainTargetAudio(stream, startup.Generation, gotKeyframe, targetAudioCodec, writeAudioSample, status) {
			if readers.activeTargetAudioEOF(done, startup.GenerationDone) {
				status.SetError(WHEPFeedTargetAudioFailed, errors.New("target audio reader closed during active WHEP feed"))
			}
			return
		}

		readEvent, sourceReady := readers.tryReadSource()
		if sourceReady {
			read := readEvent.result
			if !stream.IsPublisherGeneration(startup.Generation) {
				return
			}
			if read.Overwritten > 0 {
				overwritten := read.Overwritten
				if !stream.IsPublisherGeneration(startup.Generation) {
					return
				}
				action := "continue_audio"
				// Source overwrite resets direct audio pacing as well as video
				// pacing. Transcoded audio has an independent target reader and
				// must keep its own clock intact.
				if !needsTranscode {
					lastAudioDTS = 0
				}
				status.recordSourceOverwrite(uint64(overwritten))
				if video != nil {
					action = "wait_keyframe"
					video.RequestKeyframe()
					status.beginVideoRecovery()
					lastVideoDTS = 0
					lastSentVideoDTS = -1
					maxSentVideoPTS = 0
					paceBaseWall = time.Time{}
					paceBaseDTS = 0
					if !refreshVideoParameterSets() {
						return
					}
				} else {
					status.recordDroppedAudio(uint64(overwritten))
				}
				slog.Warn("whep: ring overwritten",
					"protocol", "whep",
					"reader", "source",
					"overwritten", read.Overwritten,
					"action", action,
				)
				continue
			}
			frame := read.Value
			if frame.MediaType.IsAudio() {
				if !needsTranscode && gotKeyframe {
					if !writeAudioSample(frame) {
						return
					}
				}
				continue
			}

			if frame.MediaType.IsVideo() {
				// Realtime and empty-cache Live modes discard frames until the
				// first live keyframe.
				if !gotKeyframe && frame.FrameType == avframe.FrameTypeKeyframe {
					gotKeyframe = true
					slog.Info("whep: got first live keyframe", "module", "webrtc", "mode", mode)
				} else if !gotKeyframe {
					status.RecordVideo(false)
					continue
				}

				// DTS-based pacing: sleep if we're sending video faster than real-time.
				paceVideo := video == nil || !video.NeedsKeyframe() || frame.FrameType == avframe.FrameTypeKeyframe
				if paceVideo && frame.DTS > 0 {
					if paceBaseWall.IsZero() {
						paceBaseWall = time.Now()
						paceBaseDTS = frame.DTS
					} else {
						dtsDelta := time.Duration(frame.DTS-paceBaseDTS) * time.Millisecond
						targetTime := paceBaseWall.Add(dtsDelta)
						sleepDur := time.Until(targetTime)

						switch dtsPaceAction(sleepDur) {
						case "sleep":
							timer := time.NewTimer(sleepDur)
							select {
							case <-timer.C:
							case <-done:
								timer.Stop()
								return
							case <-startup.GenerationDone:
								timer.Stop()
								return
							}
						case "reset":
							paceBaseWall = time.Now()
							paceBaseDTS = frame.DTS
						}
						// "deliver": behind real-time, send immediately.
						// GCC pacer smooths the RTP output.
					}
				}
				writeVideoSample(frame)
				if isWHEPFeedTerminal(status.Snapshot().State) {
					return
				}
			}
			continue
		}
		if !readers.wait(done, startup.GenerationDone) {
			if readers.activeTargetAudioEOF(done, startup.GenerationDone) {
				status.SetError(WHEPFeedTargetAudioFailed, errors.New("target audio reader closed during active WHEP feed"))
			}
			return
		}
	}
}

// whepLiveSnapshot captures the cached GOP and source-ring cursor together.
// When audio will be transcoded, the source cache contributes video only;
// source-codec audio cannot be packetized on the negotiated target track.
func whepLiveSnapshot(snapshot core.StreamStartupSnapshot, needsTranscode bool) []*avframe.AVFrame {
	frames := snapshot.ReplayFrames
	if !needsTranscode {
		return frames
	}

	videoOnly := make([]*avframe.AVFrame, 0, len(frames))
	for _, frame := range frames {
		if frame.MediaType.IsVideo() {
			videoOnly = append(videoOnly, frame)
		}
	}
	return videoOnly
}

type whepFeedReaders struct {
	source               *util.RingReader[*avframe.AVFrame]
	targetAudio          *util.RingReader[*avframe.AVFrame]
	release              func()
	waitOnce             sync.Once
	closeOnce            sync.Once
	lifecycleMu          sync.Mutex
	closed               bool
	waitContext          context.Context
	waitCancel           func()
	done                 <-chan struct{}
	generationDone       <-chan struct{}
	waitGroup            sync.WaitGroup
	sourceEvents         chan whepReaderEvent
	audioEvents          chan whepReaderEvent
	sourceReady          chan struct{}
	audioReady           chan struct{}
	sourcePermit         chan struct{}
	audioPermit          chan struct{}
	sourceTerminalNotify chan struct{}
	audioTerminalNotify  chan struct{}
	pendingSource        *whepReaderEvent
	pendingAudio         *whepReaderEvent
	sourceTerminal       atomic.Uint32
	audioTerminal        atomic.Uint32
}

type whepReaderKind uint8

const (
	whepReaderSource whepReaderKind = iota + 1
	whepReaderTargetAudio
)

func (k whepReaderKind) String() string {
	switch k {
	case whepReaderSource:
		return "source"
	case whepReaderTargetAudio:
		return "target_audio"
	default:
		return "unknown"
	}
}

type whepReaderTerminalCause uint8

const (
	whepReaderTerminalNone whepReaderTerminalCause = iota
	whepReaderTerminalEOF
	whepReaderTerminalCanceled
	whepReaderTerminalGenerationEnded
)

func (c whepReaderTerminalCause) String() string {
	switch c {
	case whepReaderTerminalNone:
		return "none"
	case whepReaderTerminalEOF:
		return "eof"
	case whepReaderTerminalCanceled:
		return "canceled"
	case whepReaderTerminalGenerationEnded:
		return "generation_ended"
	default:
		return "unknown"
	}
}

type whepReaderEvent struct {
	reader   whepReaderKind
	result   util.RingReadResult[*avframe.AVFrame]
	terminal whepReaderTerminalCause
	ack      func()
}

var (
	errWHEPReaderCanceled        = errors.New("whep reader canceled")
	errWHEPReaderGenerationEnded = errors.New("whep reader generation ended")
)

func newWHEPFeedReaders(stream *core.Stream, snapshot core.StreamStartupSnapshot, needsTranscode bool, targetAudioCodec avframe.CodecType) (*whepFeedReaders, error) {
	readers := &whepFeedReaders{source: stream.RingBuffer().NewReaderAt(snapshot.LiveCursor)}
	if needsTranscode {
		tm := stream.TranscodeManager()
		if tm == nil {
			readers.Close()
			return nil, errors.New("audio transcoding is not configured for this stream")
		}
		reader, release, err := tm.GetOrCreateAudioReaderAt(targetAudioCodec, snapshot)
		if err != nil {
			readers.Close()
			return nil, fmt.Errorf("audio transcode setup failed: %w", err)
		}
		readers.targetAudio = reader
		readers.release = release
	}
	return readers, nil
}

func (r *whepFeedReaders) Close() {
	r.closeOnce.Do(func() {
		r.lifecycleMu.Lock()
		r.closed = true
		if r.waitCancel != nil {
			r.waitCancel()
		}
		if r.source != nil {
			r.source.Close()
		}
		if r.targetAudio != nil {
			r.targetAudio.Close()
		}
		r.lifecycleMu.Unlock()
		r.waitGroup.Wait()
		if r.release != nil {
			r.release()
		}
	})
}

func (r *whepFeedReaders) drainTargetAudio(stream *core.Stream, generation uint64, ready bool, targetCodec avframe.CodecType, writeAudio func(*avframe.AVFrame) bool, status *whepFeedStatus) bool {
	if r.targetAudio == nil {
		return stream.IsPublisherGeneration(generation)
	}
	r.ensureWaiters()
	for {
		readEvent, available := r.tryReadTargetAudio()
		if !available {
			if r.targetAudioTerminalCause() == whepReaderTerminalEOF {
				return false
			}
			return stream.IsPublisherGeneration(generation)
		}
		read := readEvent.result
		if !stream.IsPublisherGeneration(generation) {
			return false
		}
		if read.Overwritten > 0 {
			overwritten := read.Overwritten
			if status != nil {
				status.recordDroppedAudio(uint64(overwritten))
			}
			slog.Warn("whep: ring overwritten",
				"protocol", "whep",
				"reader", "target_audio",
				"overwritten", read.Overwritten,
				"action", "continue_audio",
			)
			continue
		}
		frame := read.Value
		if ready && frame.MediaType.IsAudio() && whepAudioFrameAllowed(frame, targetCodec) {
			if !writeAudio(frame) {
				return false
			}
		}
	}
}

func (r *whepFeedReaders) startWaiters(done, generationDone <-chan struct{}) {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.closed {
		return
	}
	r.waitOnce.Do(func() {
		ctx, cancel := context.WithCancelCause(context.Background())
		r.waitContext = ctx
		r.done = done
		r.generationDone = generationDone
		r.waitCancel = func() { cancel(errWHEPReaderCanceled) }
		if r.source != nil {
			r.sourceEvents = make(chan whepReaderEvent)
			r.sourceReady = make(chan struct{})
			r.sourcePermit = make(chan struct{})
			r.sourceTerminalNotify = make(chan struct{}, 1)
			r.waitGroup.Add(1)
			go func() {
				defer r.waitGroup.Done()
				pumpWHEPReaderGated(ctx, whepReaderSource, r.source, r.sourceEvents, &r.sourceTerminal, r.sourceReady, r.sourcePermit, r.sourceTerminalNotify, generationDone)
			}()
		}
		if r.targetAudio != nil {
			r.audioEvents = make(chan whepReaderEvent)
			r.audioReady = make(chan struct{})
			r.audioPermit = make(chan struct{})
			r.audioTerminalNotify = make(chan struct{}, 1)
			r.waitGroup.Add(1)
			go func() {
				defer r.waitGroup.Done()
				pumpWHEPReaderGated(ctx, whepReaderTargetAudio, r.targetAudio, r.audioEvents, &r.audioTerminal, r.audioReady, r.audioPermit, r.audioTerminalNotify, generationDone)
			}()
		}
		if done != nil || generationDone != nil {
			r.waitGroup.Add(1)
			go func() {
				defer r.waitGroup.Done()
				if generationDone != nil {
					select {
					case <-generationDone:
						cancel(errWHEPReaderGenerationEnded)
						return
					default:
					}
				}
				select {
				case <-done:
					select {
					case <-generationDone:
						cancel(errWHEPReaderGenerationEnded)
					default:
						cancel(errWHEPReaderCanceled)
					}
				case <-generationDone:
					cancel(errWHEPReaderGenerationEnded)
				case <-ctx.Done():
				}
			}()
		}
	})
}

func (r *whepFeedReaders) ensureWaiters() {
	r.startWaiters(nil, nil)
}

func pumpWHEPReader(ctx context.Context, readerKind whepReaderKind, reader *util.RingReader[*avframe.AVFrame], events chan<- whepReaderEvent, terminal *atomic.Uint32) {
	pumpWHEPReaderGated(ctx, readerKind, reader, events, terminal, nil, nil, nil)
}

func pumpWHEPReaderGated(ctx context.Context, readerKind whepReaderKind, reader *util.RingReader[*avframe.AVFrame], events chan<- whepReaderEvent, terminal *atomic.Uint32, ready chan<- struct{}, permit <-chan struct{}, terminalNotify chan<- struct{}, generationDone ...<-chan struct{}) {
	defer close(events)
	var generationEnd <-chan struct{}
	if len(generationDone) > 0 {
		generationEnd = generationDone[0]
	}
	publishTerminal := func(cause whepReaderTerminalCause) {
		terminal.Store(uint32(cause))
		if terminalNotify != nil {
			select {
			case terminalNotify <- struct{}{}:
			default:
			}
		}
		select {
		case events <- whepReaderEvent{reader: readerKind, terminal: cause}:
		default:
		}
	}
	terminalCause := func() whepReaderTerminalCause {
		select {
		case <-generationEnd:
			return whepReaderTerminalGenerationEnded
		default:
		}
		if errors.Is(context.Cause(ctx), errWHEPReaderGenerationEnded) {
			return whepReaderTerminalGenerationEnded
		}
		if ctx.Err() != nil {
			return whepReaderTerminalCanceled
		}
		return whepReaderTerminalEOF
	}
	for {
		var read util.RingReadResult[*avframe.AVFrame]
		if ready != nil {
			if !reader.WaitContext(ctx) {
				publishTerminal(terminalCause())
				return
			}
			select {
			case ready <- struct{}{}:
			case <-ctx.Done():
				return
			}
			select {
			case <-permit:
			case <-ctx.Done():
				return
			}
			read = reader.TryReadResult()
		} else {
			read = reader.ReadResultContext(ctx)
		}
		if !read.OK {
			publishTerminal(terminalCause())
			return
		}
		if ctx.Err() != nil {
			return
		}
		event := whepReaderEvent{reader: readerKind, result: read}
		if read.Overwritten > 0 {
			reader.AdvanceToLive()
		}
		acknowledged := make(chan struct{})
		var acknowledgeOnce sync.Once
		event.ack = func() {
			acknowledgeOnce.Do(func() { close(acknowledged) })
		}
		select {
		case events <- event:
		case <-ctx.Done():
			return
		}
		select {
		case <-acknowledged:
		case <-ctx.Done():
			return
		}
	}
}

func acknowledgeWHEPReaderEvent(event *whepReaderEvent) {
	if event == nil || event.ack == nil {
		return
	}
	event.ack()
	event.ack = nil
}

func (r *whepFeedReaders) activeTargetAudioEOF(done, generationDone <-chan struct{}) bool {
	r.startWaiters(done, generationDone)
	if r.audioEvents == nil {
		return false
	}
	if r.pendingAudio == nil && r.targetAudioTerminalCause() == whepReaderTerminalNone {
		if event, ok := r.tryReadTargetAudio(); ok {
			// EOF probing must not consume media. The feed loop owns the
			// merge order, so preserve the event for drainTargetAudio.
			r.pendingAudio = &event
		}
	}
	if r.targetAudioTerminalCause() != whepReaderTerminalEOF {
		return false
	}
	return whepReaderStopCause(done, generationDone) == whepReaderTerminalNone
}

func (r *whepFeedReaders) wait(done, generationDone <-chan struct{}) bool {
	r.startWaiters(done, generationDone)
	if r.pendingSource != nil || r.pendingAudio != nil {
		return true
	}
	select {
	case <-done:
		return false
	case <-generationDone:
		return false
	case <-r.sourceTerminalNotify:
		return false
	case <-r.audioTerminalNotify:
		return false
	case <-r.sourceReady:
		return r.receiveSourceAfterReady(done, generationDone)
	case <-r.audioReady:
		return r.receiveAudioAfterReady(done, generationDone)
	case event, ok := <-r.sourceEvents:
		event, ok = r.acceptSourceEvent(event, ok)
		if !ok {
			return false
		}
		r.pendingSource = &event
		return true
	case event, ok := <-r.audioEvents:
		event, ok = r.acceptTargetAudioEvent(event, ok)
		if !ok {
			return false
		}
		r.pendingAudio = &event
		return true
	}
}

func (r *whepFeedReaders) receiveSourceAfterReady(done, generationDone <-chan struct{}) bool {
	if !r.grantRead(r.sourcePermit, done, generationDone) {
		return false
	}
	event, ok := <-r.sourceEvents
	event, ok = r.acceptSourceEvent(event, ok)
	if !ok {
		return false
	}
	r.pendingSource = &event
	return true
}

func (r *whepFeedReaders) receiveAudioAfterReady(done, generationDone <-chan struct{}) bool {
	if !r.grantRead(r.audioPermit, done, generationDone) {
		return false
	}
	event, ok := <-r.audioEvents
	event, ok = r.acceptTargetAudioEvent(event, ok)
	if !ok {
		return false
	}
	r.pendingAudio = &event
	return true
}

func (r *whepFeedReaders) grantRead(permit chan<- struct{}, done, generationDone <-chan struct{}) bool {
	r.lifecycleMu.Lock()
	if done == nil {
		done = r.done
	}
	if generationDone == nil {
		generationDone = r.generationDone
	}
	var lifecycleDone <-chan struct{}
	if r.waitContext != nil {
		lifecycleDone = r.waitContext.Done()
	}
	r.lifecycleMu.Unlock()
	select {
	case permit <- struct{}{}:
		return true
	case <-done:
		return false
	case <-generationDone:
		return false
	case <-lifecycleDone:
		return false
	}
}

func (r *whepFeedReaders) tryReadSource() (whepReaderEvent, bool) {
	r.ensureWaiters()
	if r.pendingSource != nil {
		event := *r.pendingSource
		r.pendingSource = nil
		acknowledgeWHEPReaderEvent(&event)
		return event, true
	}
	select {
	case <-r.sourceReady:
		if !r.grantRead(r.sourcePermit, nil, nil) {
			return whepReaderEvent{}, false
		}
		event, ok := <-r.sourceEvents
		event, ok = r.acceptSourceEvent(event, ok)
		if ok {
			acknowledgeWHEPReaderEvent(&event)
		}
		return event, ok
	default:
	}
	select {
	case event, ok := <-r.sourceEvents:
		event, ok = r.acceptSourceEvent(event, ok)
		if ok {
			acknowledgeWHEPReaderEvent(&event)
		}
		return event, ok
	default:
		return whepReaderEvent{}, false
	}
}

func (r *whepFeedReaders) tryReadTargetAudio() (whepReaderEvent, bool) {
	r.ensureWaiters()
	if r.pendingAudio != nil {
		event := *r.pendingAudio
		r.pendingAudio = nil
		acknowledgeWHEPReaderEvent(&event)
		return event, true
	}
	select {
	case <-r.audioReady:
		if !r.grantRead(r.audioPermit, nil, nil) {
			return whepReaderEvent{}, false
		}
		event, ok := <-r.audioEvents
		event, ok = r.acceptTargetAudioEvent(event, ok)
		if ok {
			acknowledgeWHEPReaderEvent(&event)
		}
		return event, ok
	default:
	}
	select {
	case event, ok := <-r.audioEvents:
		event, ok = r.acceptTargetAudioEvent(event, ok)
		if ok {
			acknowledgeWHEPReaderEvent(&event)
		}
		return event, ok
	default:
		return whepReaderEvent{}, false
	}
}

func (r *whepFeedReaders) acceptSourceEvent(event whepReaderEvent, ok bool) (whepReaderEvent, bool) {
	if !ok {
		return whepReaderEvent{}, false
	}
	if event.terminal != whepReaderTerminalNone {
		r.sourceTerminal.Store(uint32(event.terminal))
		return whepReaderEvent{}, false
	}
	return event, true
}

func (r *whepFeedReaders) acceptTargetAudioEvent(event whepReaderEvent, ok bool) (whepReaderEvent, bool) {
	if !ok {
		return whepReaderEvent{}, false
	}
	if event.terminal != whepReaderTerminalNone {
		r.audioTerminal.Store(uint32(event.terminal))
		return whepReaderEvent{}, false
	}
	return event, true
}

func (r *whepFeedReaders) targetAudioTerminalCause() whepReaderTerminalCause {
	switch r.audioTerminal.Load() {
	case uint32(whepReaderTerminalEOF):
		return whepReaderTerminalEOF
	case uint32(whepReaderTerminalCanceled):
		return whepReaderTerminalCanceled
	case uint32(whepReaderTerminalGenerationEnded):
		return whepReaderTerminalGenerationEnded
	default:
		return whepReaderTerminalNone
	}
}

func whepReaderStopCause(done, generationDone <-chan struct{}) whepReaderTerminalCause {
	select {
	case <-done:
		return whepReaderTerminalCanceled
	default:
	}
	select {
	case <-generationDone:
		return whepReaderTerminalGenerationEnded
	default:
	}
	return whepReaderTerminalNone
}

func whepInitialKeyframeReady(mode string, cacheKeyframeSent bool) bool {
	return mode == "live" && cacheKeyframeSent
}

// Audio-only streams have no video keyframe to wait for. They can start
// delivering audio as soon as the peer connection is ready.
func whepInitialMediaReady(mode string, cacheKeyframeSent, hasVideo bool) bool {
	if !hasVideo {
		return true
	}
	return whepInitialKeyframeReady(mode, cacheKeyframeSent)
}

func shouldDropWHEPVideoFrame(codec avframe.CodecType, frame *avframe.AVFrame, maxSentPTS int64) bool {
	return codec == avframe.CodecH264 && frame.FrameType != avframe.FrameTypeKeyframe && frame.PTS < maxSentPTS
}

func whepAudioFrameAllowed(frame *avframe.AVFrame, targetCodec avframe.CodecType) bool {
	return frame.Codec == targetCodec && frame.FrameType != avframe.FrameTypeSequenceHeader && len(frame.Payload) > 0
}

func whepParameterSetsReady(codec avframe.CodecType, configuration []byte) bool {
	annexB := pkgrtp.VideoToAnnexB(codec, configuration, true)
	switch codec {
	case avframe.CodecH264:
		return len(pkgrtp.BuildAVCDecoderConfig(annexB)) > 0
	case avframe.CodecH265:
		return len(h265.BuildHVCCDecoderConfig(annexB)) > 0
	default:
		return true
	}
}

// dtsPaceAction returns the action the feed loop should take based on
// how far ahead or behind the DTS pacer is relative to wall clock.
//
//   - "sleep":   ahead of real-time, sleep to match DTS pace
//   - "deliver": behind or on time, deliver immediately (pacer smooths output)
//   - "reset":   DTS discontinuity (>1s gap), reset pace base
func dtsPaceAction(sleepDur time.Duration) string {
	if sleepDur >= time.Second || sleepDur < -time.Second {
		return "reset"
	}
	if sleepDur > 0 {
		return "sleep"
	}
	return "deliver"
}

// codecToMime maps avframe CodecType to WebRTC MIME type.
func codecToMime(codec avframe.CodecType) string {
	switch codec {
	case avframe.CodecH264:
		return webrtc.MimeTypeH264
	case avframe.CodecH265:
		return webrtc.MimeTypeH265
	case avframe.CodecVP8:
		return webrtc.MimeTypeVP8
	case avframe.CodecVP9:
		return webrtc.MimeTypeVP9
	case avframe.CodecAV1:
		return webrtc.MimeTypeAV1
	case avframe.CodecOpus:
		return webrtc.MimeTypeOpus
	case avframe.CodecG711U:
		return webrtc.MimeTypePCMU
	case avframe.CodecG711A:
		return webrtc.MimeTypePCMA
	default:
		return ""
	}
}
