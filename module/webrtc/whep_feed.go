package webrtc

import (
	"log/slog"
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
func whepFeedLoop(stream *core.Stream, video, audio *TrackSender, done <-chan struct{}, connected <-chan struct{}, mode string, videoCodec avframe.CodecType, targetAudioCodec avframe.CodecType, bwe cc.BandwidthEstimator) {
	// Wait for ICE+DTLS to complete before sending media.
	select {
	case <-connected:
		slog.Info("peer connected, starting media feed", "module", "webrtc", "mode", mode)
	case <-done:
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
	var sourceAudioCodec avframe.CodecType
	if pub := stream.Publisher(); pub != nil {
		sourceAudioCodec = pub.MediaInfo().AudioCodec
	}
	needsTranscode := targetAudioCodec != sourceAudioCodec && sourceAudioCodec != 0

	// Track the last DTS to compute sample durations.
	var lastVideoDTS, lastAudioDTS int64

	// For H264/H265: cache parameter sets (SPS/PPS or VPS/SPS/PPS) in Annex-B
	// format to prepend to keyframes. Chromium requires parameter sets to share
	// the same RTP timestamp as the IDR NAL.
	var paramSetBuf []byte
	needsAnnexB := videoCodec == avframe.CodecH264 || videoCodec == avframe.CodecH265
	if needsAnnexB {
		if sh := stream.VideoSeqHeader(); sh != nil {
			paramSetBuf = pkgrtp.ToAnnexB(sh.Payload, true)
		}
	}

	// B-frame drop: Chrome's WebRTC H.264 decoder does not perform B-frame
	// reordering (it's designed for Baseline profile). Sending B-frames
	// causes either visual jitter (DTS-order) or mosaic corruption
	// (PTS-order). We drop B-frames and send only I/P reference frames.
	//
	// Detection: track the highest PTS sent. Any frame whose PTS is below
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
	// B-frame drop: frames with PTS < maxSentVideoPTS are silently dropped.
	writeVideoSample := func(frame *avframe.AVFrame) {
		if video == nil {
			return
		}

		// SequenceHeader: cache parameter sets, do not send as a sample.
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			if needsAnnexB {
				paramSetBuf = pkgrtp.ToAnnexB(frame.Payload, true)
			}
			return
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
				return
			}
			video.ClearNeedsKeyframe()
			slog.Debug("PLI resync: sending keyframe", "module", "webrtc", "bytes", len(frame.Payload))
		}

		// Drop B-frames: if this frame's display time (PTS) is earlier than
		// a frame we already sent, it's a B-frame that the WebRTC decoder
		// cannot handle. Drop it silently but still track DTS for the pacer.
		if frame.FrameType != avframe.FrameTypeKeyframe && frame.PTS < maxSentVideoPTS {
			if frame.DTS > 0 {
				lastVideoDTS = frame.DTS
			}
			return
		}

		var payload []byte
		if needsAnnexB {
			// H264/H265: convert AVCC/HVCC length-prefixed NALs to Annex-B.
			payload = pkgrtp.ToAnnexB(frame.Payload, false)
			if len(payload) == 0 {
				return
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
				return
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
			return
		}
	}

	// Compute fixed audio frame duration for transcoded Opus.
	// Using a fixed duration avoids any DTS-delta precision issues and
	// ensures RTP timestamps advance by exactly 960 ticks per frame
	// (20ms × 48kHz), matching the actual Opus content duration.
	var fixedAudioDur time.Duration
	if needsTranscode && targetAudioCodec == avframe.CodecOpus {
		fixedAudioDur = 20 * time.Millisecond // 960 samples / 48kHz
	}

	// writeAudioSample writes audio frames. AAC from RTMP is not WebRTC
	// compatible (no codecToMime mapping), so audioSender will be nil in
	// that case and this is a no-op.
	writeAudioSample := func(frame *avframe.AVFrame) {
		if audio == nil {
			return
		}

		// Skip audio sequence headers (e.g. AudioSpecificConfig for AAC).
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			return
		}

		payload := frame.Payload
		if len(payload) == 0 {
			return
		}

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

	// Live mode: send GOP cache so the subscriber gets an immediate keyframe,
	// paced at 10x real-time speed to avoid flooding the browser's jitter buffer.
	//
	// When audio transcoding is needed, skip the GOP cache entirely.
	// The GOP cache only contains source-codec audio (e.g. AAC) which cannot
	// be sent over the Opus track. Sending video-only from the cache causes
	// A/V desync: the browser buffers 1-2s of video before any audio arrives,
	// making audio lag behind by the GOP cache duration.
	// Falling back to realtime behavior (wait for live keyframe) ensures both
	// audio and video start from the same point in time.
	if mode == "live" && needsTranscode {
		slog.Info("live mode with audio transcoding: skipping GOP cache to preserve A/V sync",
			"module", "webrtc")
		mode = "realtime" // fall back to realtime behavior
	}

	if mode == "live" {
		gopCache := stream.GOPCache()
		var prevDTS int64
		for _, frame := range gopCache {
			if frame.MediaType.IsVideo() {
				writeVideoSample(frame)
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

	// Capture write position AFTER GOP cache so the live reader starts at
	// the current position, not where we were before the cache was sent.
	// This avoids DTS discontinuities between cached and live frames.
	startPos := stream.RingBuffer().WriteCursor()

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

	// Set up the live reader. When audio transcoding is needed, use
	// TranscodeManager's reader which provides video passthrough and
	// transcoded audio frames in the target codec.
	var reader *util.RingReader[*avframe.AVFrame]
	var transcodeRelease func()
	if needsTranscode {
		if tm := stream.TranscodeManager(); tm != nil {
			var err error
			reader, transcodeRelease, err = tm.GetOrCreateReader(targetAudioCodec)
			if err != nil {
				slog.Warn("whep: audio transcode failed, video only", "error", err)
				needsTranscode = false
				reader = stream.RingBuffer().NewReaderAt(startPos)
			}
		} else {
			needsTranscode = false
			reader = stream.RingBuffer().NewReaderAt(startPos)
		}
	} else {
		reader = stream.RingBuffer().NewReaderAt(startPos)
	}
	if transcodeRelease != nil {
		defer transcodeRelease()
	}

	// Live frame loop: start reading only NEW frames (after snapshot).
	// In realtime mode, skip all frames until the first video keyframe
	// arrives, then start sending from that keyframe onward.
	gotKeyframe := mode == "live" // live mode already has GOP cache, no need to wait
	for {
		frame, ok := reader.TryRead()
		if ok {
			// Realtime mode: discard frames until first keyframe.
			if !gotKeyframe {
				if frame.MediaType.IsVideo() && frame.FrameType == avframe.FrameTypeKeyframe {
					gotKeyframe = true
					slog.Info("realtime mode: got first keyframe", "module", "webrtc")
				} else {
					continue
				}
			}

			// Audio: deliver immediately without DTS pacing.
			// Audio has its own fixed duration (e.g. 20ms for Opus) that drives
			// RTP timestamp advancement. Chrome's audio jitter buffer handles
			// arrival variance independently. Pacing audio would delay video
			// frame reads in this goroutine, causing video jitter.
			if frame.MediaType.IsAudio() {
				writeAudioSample(frame)
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
						}
					case "reset":
						paceBaseWall = time.Now()
						paceBaseDTS = frame.DTS
					}
					// "deliver": behind real-time, send immediately.
					// GCC pacer smooths the RTP output.
				}
			}

			if frame.MediaType.IsVideo() {
				writeVideoSample(frame)
			}
			continue
		}
		select {
		case <-done:
			return
		case <-reader.Signal():
		}
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
