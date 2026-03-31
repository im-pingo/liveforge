package webrtc

import (
	"log/slog"
	"time"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
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
func whepFeedLoop(stream *core.Stream, video, audio *TrackSender, done <-chan struct{}, connected <-chan struct{}, mode string, videoCodec avframe.CodecType, bwe cc.BandwidthEstimator) {
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

	// writeVideoSample writes one video frame to the WebRTC track.
	//
	// Codec-specific handling:
	//   H264/H265: AVCC/HVCC → Annex-B conversion; parameter sets prepended
	//              to keyframes so the decoder sees them in the same access unit.
	//   VP8/VP9/AV1: raw frame data passed directly to pion's packetizer.
	//
	// PLI/FIR resync: inter-frames are skipped until the next keyframe.
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
		// IMPORTANT: always advance lastVideoDTS even for skipped frames so
		// the first keyframe after resync gets a normal ~40ms duration instead
		// of a multi-second gap that corrupts Chrome's jitter buffer timing.
		if video.NeedsKeyframe() {
			if frame.FrameType != avframe.FrameTypeKeyframe {
				if frame.DTS > 0 {
					lastVideoDTS = frame.DTS
				}
				return
			}
			video.ClearNeedsKeyframe()
			slog.Debug("PLI resync: sending keyframe", "module", "webrtc", "bytes", len(frame.Payload))
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

		// Compute duration from DTS delta.
		duration := time.Duration(0)
		if lastVideoDTS > 0 && frame.DTS > lastVideoDTS {
			duration = time.Duration(frame.DTS-lastVideoDTS) * time.Millisecond
		} else {
			duration = 40 * time.Millisecond // ~25fps default
		}
		if frame.DTS > 0 {
			lastVideoDTS = frame.DTS
		}

		if err := video.WriteSample(media.Sample{
			Data:     payload,
			Duration: duration,
		}); err != nil {
			return
		}
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

		duration := time.Duration(0)
		if lastAudioDTS > 0 && frame.DTS > lastAudioDTS {
			duration = time.Duration(frame.DTS-lastAudioDTS) * time.Millisecond
		} else {
			duration = 23 * time.Millisecond // ~44100Hz AAC frame
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

	// Live frame loop: start reading only NEW frames (after snapshot).
	// In realtime mode, skip all frames until the first video keyframe
	// arrives, then start sending from that keyframe onward.
	gotKeyframe := mode == "live" // live mode already has GOP cache, no need to wait
	reader := stream.RingBuffer().NewReaderAt(startPos)
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

			// DTS-based pacing: sleep if we're sending faster than real-time.
			if frame.DTS > 0 {
				if paceBaseWall.IsZero() {
					paceBaseWall = time.Now()
					paceBaseDTS = frame.DTS
				} else {
					dtsDelta := time.Duration(frame.DTS-paceBaseDTS) * time.Millisecond
					targetTime := paceBaseWall.Add(dtsDelta)
					sleepDur := time.Until(targetTime)

					if sleepDur > 0 && sleepDur < time.Second {
						timer := time.NewTimer(sleepDur)
						select {
						case <-timer.C:
						case <-done:
							timer.Stop()
							return
						}
					} else if sleepDur < -500*time.Millisecond {
						// Too far behind (>500ms): reset base to prevent permanent lag.
						paceBaseWall = time.Now()
						paceBaseDTS = frame.DTS
					}
				}
			}

			if frame.MediaType.IsVideo() {
				writeVideoSample(frame)
			} else if frame.MediaType.IsAudio() {
				writeAudioSample(frame)
			}
			continue
		}
		select {
		case <-done:
			return
		case <-stream.RingBuffer().Signal():
		}
	}
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
