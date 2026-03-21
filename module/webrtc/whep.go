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

	// drainRTCP consumes RTCP packets from an RTPSender to prevent the
	// sender's read buffer from filling up and blocking WriteSample.
	drainRTCP := func(sender *webrtc.RTPSender) {
		go func() {
			buf := make([]byte, 1500)
			for {
				if _, _, err := sender.Read(buf); err != nil {
					return
				}
			}
		}()
	}

	if info.HasVideo() {
		mime := codecToMime(info.VideoCodec)
		if mime != "" {
			vt, err := webrtc.NewTrackLocalStaticSample(
				webrtc.RTPCodecCapability{MimeType: mime, ClockRate: 90000},
				"video", "liveforge",
			)
			if err == nil {
				if sender, err := pc.AddTrack(vt); err == nil {
					videoTrack = vt
					drainRTCP(sender)
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
				if sender, err := pc.AddTrack(at); err == nil {
					audioTrack = at
					drainRTCP(sender)
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

	// Determine playback mode from query parameter.
	// "realtime" (default): skip GOP cache, wait for next live keyframe.
	// "live": send GOP cache like other protocols for immediate playback.
	mode := r.URL.Query().Get("mode")
	if mode != "live" {
		mode = "realtime"
	}

	// Start the feed goroutine. It will wait for the connection to be
	// established before sending any media data.
	go whepFeedLoop(stream, videoTrack, audioTrack, sess.done, connected, mode)

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
//
// mode controls startup behavior:
//   - "realtime": skip GOP cache, read live frames, discard until first keyframe.
//   - "live": send GOP cache (paced at 10x speed), then live frames.
func whepFeedLoop(stream *core.Stream, videoTrack, audioTrack *webrtc.TrackLocalStaticSample, done <-chan struct{}, connected <-chan struct{}, mode string) {
	// Wait for ICE+DTLS to complete before sending media.
	select {
	case <-connected:
		log.Printf("[webrtc] peer connected, starting media feed (mode=%s)", mode)
	case <-done:
		return
	}

	// Snapshot write position so the live reader starts from here.
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
						time.Sleep(sleep)
					}
				}
			}
			if frame.DTS > 0 {
				prevDTS = frame.DTS
			}
		}
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
					log.Printf("[webrtc] realtime mode: got first keyframe, starting send")
				} else {
					continue
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
