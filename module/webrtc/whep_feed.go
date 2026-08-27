package webrtc

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/util"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

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
func whepFeedLoop(stream *core.Stream, startup core.StreamStartupSnapshot, video, audio *TrackSender, done <-chan struct{}, connected <-chan struct{}, mode string, targetAudioCodec avframe.CodecType, bwe cc.BandwidthEstimator) {
	// Wait for ICE+DTLS to complete before sending media.
	select {
	case <-connected:
		slog.Info("peer connected, starting media feed", "module", "webrtc", "mode", mode)
	case <-done:
		return
	case <-startup.GenerationDone:
		return
	}

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
			paramSetBuf = pkgrtp.VideoToAnnexB(videoCodec, sh.Payload, true)
		}
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
				paramSetBuf = pkgrtp.VideoToAnnexB(videoCodec, frame.Payload, true)
			}
			return false
		}

		// PLI/FIR resync: skip inter-frames until the next keyframe.
		// Reset DTS tracker so the first keyframe after resync
		// gets a normal ~40ms duration instead of a multi-second gap.
		if video.NeedsKeyframe() {
			if frame.FrameType != avframe.FrameTypeKeyframe {
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

		if err := video.WriteSample(media.Sample{
			Data:     payload,
			Duration: duration,
		}); err != nil {
			return false
		}
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
	writeAudioSample := func(frame *avframe.AVFrame) {
		if audio == nil {
			return
		}

		if !whepAudioFrameAllowed(frame, targetAudioCodec) {
			return
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

		if err := audio.WriteSample(media.Sample{
			Data:     payload,
			Duration: duration,
		}); err != nil {
			return
		}
	}

	var gopCache []*avframe.AVFrame
	if mode == "live" {
		gopCache = whepLiveSnapshot(startup, needsTranscode)
	}

	readers := newWHEPFeedReaders(stream, startup, needsTranscode, targetAudioCodec)
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
				writeAudioSample(frame)
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
	for {
		if !readers.drainTargetAudio(stream, startup.Generation, gotKeyframe, targetAudioCodec, writeAudioSample) {
			return
		}

		frame, ok := readers.source.TryRead()
		if ok {
			if !stream.IsPublisherGeneration(startup.Generation) {
				return
			}
			if frame.MediaType.IsAudio() {
				if !needsTranscode && gotKeyframe {
					writeAudioSample(frame)
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
					continue
				}

				// DTS-based pacing: sleep if we're sending video faster than real-time.
				if frame.DTS > 0 {
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
			}
			continue
		}
		if !readers.wait(done, startup.GenerationDone) {
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

	videoOnly := frames[:0]
	for _, frame := range frames {
		if frame.MediaType.IsVideo() {
			videoOnly = append(videoOnly, frame)
		}
	}
	return videoOnly
}

type whepFeedReaders struct {
	source       *util.RingReader[*avframe.AVFrame]
	targetAudio  *util.RingReader[*avframe.AVFrame]
	release      func()
	waitOnce     sync.Once
	waitCancel   context.CancelFunc
	sourceWake   chan struct{}
	audioWake    chan struct{}
	sourceClosed chan struct{}
}

func newWHEPFeedReaders(stream *core.Stream, snapshot core.StreamStartupSnapshot, needsTranscode bool, targetAudioCodec avframe.CodecType) *whepFeedReaders {
	readers := &whepFeedReaders{source: stream.RingBuffer().NewReaderAt(snapshot.LiveCursor)}
	if needsTranscode {
		if tm := stream.TranscodeManager(); tm != nil {
			reader, release, err := tm.GetOrCreateAudioReaderAt(targetAudioCodec, snapshot)
			if err != nil {
				slog.Warn("whep: audio transcode failed, video only", "error", err)
			} else {
				readers.targetAudio = reader
				readers.release = release
			}
		}
	}
	return readers
}

func (r *whepFeedReaders) Close() {
	if r.waitCancel != nil {
		r.waitCancel()
	}
	if r.release != nil {
		r.release()
	}
}

func (r *whepFeedReaders) drainTargetAudio(stream *core.Stream, generation uint64, ready bool, targetCodec avframe.CodecType, writeAudio func(*avframe.AVFrame)) bool {
	if r.targetAudio == nil {
		return stream.IsPublisherGeneration(generation)
	}
	for {
		frame, ok := r.targetAudio.TryRead()
		if !ok {
			return stream.IsPublisherGeneration(generation)
		}
		if !stream.IsPublisherGeneration(generation) {
			return false
		}
		if ready && frame.MediaType.IsAudio() && whepAudioFrameAllowed(frame, targetCodec) {
			writeAudio(frame)
		}
	}
}

func (r *whepFeedReaders) startWaiters(done, generationDone <-chan struct{}) {
	r.waitOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		r.waitCancel = cancel
		r.sourceWake = make(chan struct{}, 1)
		r.sourceClosed = make(chan struct{})
		go watchWHEPReader(ctx, r.source, r.sourceWake, r.sourceClosed)
		if r.targetAudio != nil {
			r.audioWake = make(chan struct{}, 1)
			go watchWHEPReader(ctx, r.targetAudio, r.audioWake, nil)
		}
		go func() {
			select {
			case <-done:
				cancel()
			case <-generationDone:
				cancel()
			case <-ctx.Done():
			}
		}()
	})
}

func watchWHEPReader(ctx context.Context, reader *util.RingReader[*avframe.AVFrame], wake chan<- struct{}, closed chan<- struct{}) {
	defer func() {
		if closed != nil {
			close(closed)
		}
	}()
	for reader.WaitContext(ctx) {
		select {
		case wake <- struct{}{}:
		case <-ctx.Done():
			return
		}
	}
}

func (r *whepFeedReaders) wait(done, generationDone <-chan struct{}) bool {
	r.startWaiters(done, generationDone)
	select {
	case <-done:
		return false
	case <-generationDone:
		return false
	case <-r.sourceClosed:
		return false
	case <-r.sourceWake:
		return true
	case <-r.audioWake:
		return true
	}
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
