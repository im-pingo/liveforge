package webrtc

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// handleWHEP handles POST /webrtc/whep/{path...} for WHEP playback.
func (m *Module) handleWHEP(w http.ResponseWriter, r *http.Request) {
	streamKey := r.PathValue("path")
	if streamKey == "" {
		http.Error(w, "missing stream key", http.StatusBadRequest)
		return
	}

	if !m.server.AcquireConn() {
		http.Error(w, "max connections reached", http.StatusServiceUnavailable)
		return
	}
	connAcquired := true
	releaseConn := func() {
		if connAcquired {
			connAcquired = false
			m.server.ReleaseConn()
		}
	}

	contentType := r.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/sdp") {
		releaseConn()
		http.Error(w, "Content-Type must be application/sdp", http.StatusUnsupportedMediaType)
		return
	}

	offerBytes, err := io.ReadAll(r.Body)
	if err != nil {
		releaseConn()
		http.Error(w, "failed to read offer", http.StatusBadRequest)
		return
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  string(offerBytes),
	}

	// Find the stream.
	stream, ok := m.server.StreamHub().Find(streamKey)
	if !ok || stream.State() != core.StreamStatePublishing {
		releaseConn()
		http.Error(w, "stream not found or not publishing", http.StatusNotFound)
		return
	}

	pub := stream.Publisher()
	if pub == nil {
		releaseConn()
		http.Error(w, "stream has no publisher", http.StatusNotFound)
		return
	}
	info := pub.MediaInfo()

	// Track subscriber limit.
	if err := stream.AddSubscriber("webrtc"); err != nil {
		releaseConn()
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	pc, err := m.api.NewPeerConnection(webrtc.Configuration{
		ICEServers: m.iceServersFromConfig(),
	})
	if err != nil {
		stream.RemoveSubscriber("webrtc")
		releaseConn()
		http.Error(w, "failed to create peer connection", http.StatusInternalServerError)
		return
	}

	sessionID := uuid.New().String()
	sess := newSession(sessionID, pc, streamKey, "whep", m)
	m.storeSession(sess)

	// Add local tracks for sending media using TrackLocalStaticSample.
	// This delegates RTP packetization to pion, ensuring correct payload type,
	// sequence numbers, timestamps, and RTP header extensions.
	var videoTrack, audioTrack *webrtc.TrackLocalStaticSample

	if info.HasVideo() {
		mime := codecToMime(info.VideoCodec)
		if mime != "" {
			vt, err := webrtc.NewTrackLocalStaticSample(
				webrtc.RTPCodecCapability{MimeType: mime, ClockRate: 90000},
				"video", "liveforge",
			)
			if err == nil {
				if _, err := pc.AddTrack(vt); err == nil {
					videoTrack = vt
					// Restrict the transceiver to only the codec we're sending.
					restrictTransceiverCodec(pc, vt, mime)
				}
			}
		}
	}

	if info.HasAudio() {
		mime := codecToMime(info.AudioCodec)
		if mime != "" {
			clockRate := uint32(48000)
			channels := uint16(2)
			if info.SampleRate > 0 {
				clockRate = uint32(info.SampleRate)
			}
			if info.Channels > 0 {
				channels = uint16(info.Channels)
			}
			at, err := webrtc.NewTrackLocalStaticSample(
				webrtc.RTPCodecCapability{MimeType: mime, ClockRate: clockRate, Channels: channels},
				"audio", "liveforge",
			)
			if err == nil {
				if _, err := pc.AddTrack(at); err == nil {
					audioTrack = at
					restrictTransceiverCodec(pc, at, mime)
				}
			}
		}
	}

	if videoTrack == nil && audioTrack == nil {
		sess.Close()
		stream.RemoveSubscriber("webrtc")
		releaseConn()
		http.Error(w, "no compatible tracks for WebRTC", http.StatusUnsupportedMediaType)
		return
	}

	// Signal channel for when the peer connection is ready to send media.
	connected := make(chan struct{})

	// Merge ICE state handling: log state, signal connection ready, and
	// clean up on disconnect. This replaces the handler set by newSession.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("[webrtc] WHEP session %s ICE state: %s", sessionID, state)
		switch state {
		case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
			select {
			case <-connected:
			default:
				close(connected)
			}
		case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateClosed,
			webrtc.ICEConnectionStateDisconnected:
			stream.RemoveSubscriber("webrtc")
			m.server.GetEventBus().Emit(core.EventSubscribeStop, &core.EventContext{
				StreamKey:  streamKey,
				Protocol:   "webrtc",
				RemoteAddr: r.RemoteAddr,
			})
			releaseConn()
			sess.Close()
		}
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		sess.Close()
		stream.RemoveSubscriber("webrtc")
		releaseConn()
		http.Error(w, fmt.Sprintf("set remote description: %v", err), http.StatusBadRequest)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		sess.Close()
		stream.RemoveSubscriber("webrtc")
		releaseConn()
		http.Error(w, fmt.Sprintf("create answer: %v", err), http.StatusInternalServerError)
		return
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)

	if err := pc.SetLocalDescription(answer); err != nil {
		sess.Close()
		stream.RemoveSubscriber("webrtc")
		releaseConn()
		http.Error(w, fmt.Sprintf("set local description: %v", err), http.StatusInternalServerError)
		return
	}

	<-gatherComplete

	// Start the feed goroutine. It will wait for the connection to be
	// established before sending any media data.
	go whepFeedLoop(stream, videoTrack, audioTrack, sess.done, connected)

	m.server.GetEventBus().Emit(core.EventSubscribe, &core.EventContext{
		StreamKey:  streamKey,
		Protocol:   "webrtc",
		RemoteAddr: r.RemoteAddr,
	})

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/webrtc/session/"+sessionID)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(pc.LocalDescription().SDP))

	log.Printf("[webrtc] WHEP session %s started for stream %s", sessionID, streamKey)
}

// whepFeedLoop reads AVFrames from the stream's RingBuffer and writes them
// to the WebRTC tracks via pion's WriteSample API. It waits for the peer
// connection to be established before sending any data.
func whepFeedLoop(stream *core.Stream, videoTrack, audioTrack *webrtc.TrackLocalStaticSample, done <-chan struct{}, connected <-chan struct{}) {
	// Wait for ICE+DTLS to complete before sending media.
	select {
	case <-connected:
		log.Printf("[webrtc] peer connected, starting media feed")
	case <-done:
		return
	}

	// Snapshot write position before sending GOP cache, so the live reader
	// starts right after the snapshot — no duplicate frames from GOP cache.
	startPos := stream.RingBuffer().WriteCursor()

	// Track the last DTS to compute sample durations.
	var lastVideoDTS, lastAudioDTS int64

	// Cache SPS/PPS in Annex-B format to prepend to keyframes.
	// Chromium requires SPS/PPS to share the same RTP timestamp as the IDR
	// NAL, so we cannot send them as separate WriteSample calls.
	var spsppsBuf []byte
	if sh := stream.VideoSeqHeader(); sh != nil {
		spsppsBuf = pkgrtp.ToAnnexB(sh.Payload, true)
	}

	// writeVideoSample handles H264/H265 video frames:
	// - SequenceHeader frames update the cached SPS/PPS but are NOT sent
	//   as separate samples (they would get a different RTP timestamp).
	// - Keyframes get SPS/PPS prepended so the decoder sees parameter sets
	//   in the same access unit as the IDR slice.
	// - Inter-frames are sent as-is.
	writeVideoSample := func(frame *avframe.AVFrame) {
		if videoTrack == nil {
			return
		}

		// SequenceHeader: cache the SPS/PPS, do not send as a sample.
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			spsppsBuf = pkgrtp.ToAnnexB(frame.Payload, true)
			return
		}

		payload := pkgrtp.ToAnnexB(frame.Payload, false)
		if len(payload) == 0 {
			return
		}

		// Prepend SPS/PPS to keyframes so they share the same RTP timestamp.
		if frame.FrameType == avframe.FrameTypeKeyframe && len(spsppsBuf) > 0 {
			combined := make([]byte, len(spsppsBuf)+len(payload))
			copy(combined, spsppsBuf)
			copy(combined[len(spsppsBuf):], payload)
			payload = combined
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

		if err := videoTrack.WriteSample(media.Sample{
			Data:     payload,
			Duration: duration,
		}); err != nil {
			return
		}
	}

	// writeAudioSample writes audio frames. AAC from RTMP is not WebRTC
	// compatible (no codecToMime mapping), so audioTrack will be nil in
	// that case and this is a no-op.
	writeAudioSample := func(frame *avframe.AVFrame) {
		if audioTrack == nil {
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

		if err := audioTrack.WriteSample(media.Sample{
			Data:     payload,
			Duration: duration,
		}); err != nil {
			return
		}
	}

	// Send GOP cache so the subscriber gets an immediate keyframe
	// (with SPS/PPS prepended).
	for _, frame := range stream.GOPCache() {
		if frame.MediaType.IsVideo() {
			writeVideoSample(frame)
		} else if frame.MediaType.IsAudio() {
			writeAudioSample(frame)
		}
	}

	// Real-time pacing anchor: tie first live video frame's DTS to wall clock.
	// This prevents bursts when multiple frames accumulate in the ring buffer
	// (e.g., after a goroutine scheduling delay). Frames arriving on time
	// are sent immediately; overdue frames are sent immediately without extra
	// delay.
	var anchorWall time.Time
	var anchorDTS int64

	// Live frame loop: start reading only NEW frames (after GOP cache snapshot).
	reader := stream.RingBuffer().NewReaderAt(startPos)
	for {
		frame, ok := reader.TryRead()
		if ok {
			// Pace video frames to wall-clock time to prevent UDP bursts.
			if frame.MediaType.IsVideo() && frame.FrameType != avframe.FrameTypeSequenceHeader && frame.DTS > 0 {
				if anchorDTS == 0 {
					anchorWall = time.Now()
					anchorDTS = frame.DTS
				} else {
					expectedAt := anchorWall.Add(time.Duration(frame.DTS-anchorDTS) * time.Millisecond)
					if sleep := time.Until(expectedAt); sleep > 0 && sleep < 500*time.Millisecond {
						time.Sleep(sleep)
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

// restrictTransceiverCodec finds the transceiver for the given track and
// restricts its codec preferences to only codecs matching the target MIME type.
// This ensures the SDP answer only includes the codec we intend to send,
// preventing payload type mismatches.
func restrictTransceiverCodec(pc *webrtc.PeerConnection, track webrtc.TrackLocal, targetMime string) {
	targetMimeLower := strings.ToLower(targetMime)
	for _, tr := range pc.GetTransceivers() {
		sender := tr.Sender()
		if sender == nil || sender.Track() != track {
			continue
		}
		// Get the sender's negotiated codec list and filter to matching MIME.
		params := sender.GetParameters()
		var filtered []webrtc.RTPCodecParameters
		for _, c := range params.Codecs {
			if strings.ToLower(c.MimeType) == targetMimeLower {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) > 0 {
			_ = tr.SetCodecPreferences(filtered)
		}
		return
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
