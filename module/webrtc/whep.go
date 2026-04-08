package webrtc

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
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

	// Normalize H264 fmtp in the offer so pion can match it.
	offer.SDP = normalizeH264Offer(offer.SDP)

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

	// Set GCC initial bitrate to the stream's actual bitrate (with 20% headroom)
	// so the pacer doesn't throttle at startup. The factory closure reads this
	// value when pion creates the BWE for this PeerConnection.
	if stats := stream.Stats(); stats.BitrateKbps > 0 {
		m.nextInitialBitrateMu.Lock()
		m.nextInitialBitrate = stats.BitrateKbps * 1000 * 120 / 100 // kbps→bps + 20%
		m.nextInitialBitrateMu.Unlock()
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

	// Retrieve bandwidth estimator for this PeerConnection (if GCC enabled).
	var bwe cc.BandwidthEstimator
	if m.latestBWE != nil {
		select {
		case bwe = <-m.latestBWE:
		default:
		}
	}

	sessionID := uuid.New().String()
	sess := newSession(sessionID, pc, streamKey, "whep", m)
	m.storeSession(sess)

	// Parse the offer SDP to determine which media types the client requests.
	// Only add tracks that match an m-line in the offer; adding tracks without
	// a corresponding offer m-line causes pion's SetLocalDescription to fail
	// with "codec is not supported by remote".
	var offerHasVideo, offerHasAudio bool
	var parsedSDP sdp.SessionDescription
	if err := parsedSDP.UnmarshalString(offer.SDP); err == nil {
		for _, md := range parsedSDP.MediaDescriptions {
			switch md.MediaName.Media {
			case "video":
				offerHasVideo = true
			case "audio":
				offerHasAudio = true
			}
		}
	}

	// Create TrackSenders for video and audio. Each TrackSender owns its
	// RTCP read loop, keeping RTCP dispatch independent of this handler.
	//
	// NOTE: We intentionally do NOT call SetCodecPreferences on the transceiver.
	// pion's RTPSender.Send() resolves the payload type via codecParametersFuzzySearch
	// against the MediaEngine's negotiated codec list (getRTPParametersByKind),
	// which is NOT filtered by transceiver codec preferences. Calling
	// SetCodecPreferences caused a PT mismatch: the SDP answer advertised one PT
	// but pion's Bind resolved a different PT from the global negotiated list,
	// resulting in Chrome receiving packets with an unexpected payload type.
	var videoSender, audioSender *TrackSender

	if info.HasVideo() && offerHasVideo {
		mime := codecToMime(info.VideoCodec)
		if mime != "" {
			vt, err := webrtc.NewTrackLocalStaticSample(
				webrtc.RTPCodecCapability{MimeType: mime, ClockRate: 90000},
				"video", "liveforge",
			)
			if err == nil {
				if s, err := pc.AddTrack(vt); err == nil {
					videoSender = NewTrackSender(sessionID, vt, s)
				}
			}
		}
	}

	if info.HasAudio() && offerHasAudio {
		mime := codecToMime(info.AudioCodec)
		if mime == "" && stream.TranscodeManager() != nil {
			// Publisher codec not WebRTC-compatible; transcode to Opus.
			mime = webrtc.MimeTypeOpus
		}
		if mime != "" {
			clockRate := uint32(48000)
			channels := uint16(2)
			// When transcoding, always use Opus defaults (48kHz/2ch).
			// For direct codec passthrough, use publisher's parameters.
			if codecToMime(info.AudioCodec) != "" {
				if info.SampleRate > 0 {
					clockRate = uint32(info.SampleRate)
				}
				if info.Channels > 0 {
					channels = uint16(info.Channels)
				}
			}
			at, err := webrtc.NewTrackLocalStaticSample(
				webrtc.RTPCodecCapability{MimeType: mime, ClockRate: clockRate, Channels: channels},
				"audio", "liveforge",
			)
			if err == nil {
				if s, err := pc.AddTrack(at); err == nil {
					audioSender = NewTrackSender(sessionID, at, s)
				}
			}
		}
	}

	if videoSender == nil && audioSender == nil {
		sess.Close()
		stream.RemoveSubscriber("webrtc")
		releaseConn()
		http.Error(w, "no compatible tracks for WebRTC", http.StatusUnsupportedMediaType)
		return
	}

	// PLI/FIR handling: TrackSender sets needsKeyframe flag automatically.
	// The feed loop checks this flag and skips inter-frames until the next
	// keyframe, so only one goroutine (the feed loop) writes media samples.

	// Start RTCP read loops. Each sender's loop runs independently;
	// audio sender has no PLI handler (audio has no concept of keyframes).
	if videoSender != nil {
		videoSender.Start()
	}
	if audioSender != nil {
		audioSender.Start()
	}

	// Signal channel for when the peer connection is ready to send media.
	connected := make(chan struct{})

	// Merge ICE state handling: log state, signal connection ready, and
	// clean up on disconnect. This replaces the handler set by newSession.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		slog.Debug("WHEP ICE state", "module", "webrtc", "session", sessionID, "state", state)
		switch state {
		case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
			select {
			case <-connected:
			default:
				close(connected)
			}
		case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateClosed:
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

	if err := pc.SetLocalDescription(answer); err != nil {
		sess.Close()
		stream.RemoveSubscriber("webrtc")
		releaseConn()
		http.Error(w, fmt.Sprintf("set local description: %v", err), http.StatusInternalServerError)
		return
	}

	// Wait for at least one ICE candidate or gathering complete,
	// whichever comes first. Avoids blocking on slow STUN/TURN
	// servers in LAN environments where host candidates suffice.
	gatherDone := make(chan struct{})
	var gatherOnce sync.Once
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil { // gathering complete
			gatherOnce.Do(func() { close(gatherDone) })
		}
	})
	pc.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		if state == webrtc.ICEGatheringStateComplete {
			gatherOnce.Do(func() { close(gatherDone) })
		}
	})
	select {
	case <-gatherDone:
	case <-time.After(500 * time.Millisecond):
	}

	// Determine playback mode from query parameter.
	// "realtime" (default): skip GOP cache, wait for next live keyframe.
	// "live": send GOP cache like other protocols for immediate playback.
	mode := r.URL.Query().Get("mode")
	if mode != "live" {
		mode = "realtime"
	}

	// Determine target audio codec for the feed loop.
	targetAudioCodec := info.AudioCodec
	if audioSender != nil && codecToMime(info.AudioCodec) == "" {
		targetAudioCodec = avframe.CodecOpus // transcoding to Opus
	}

	// Start the feed goroutine. It waits for ICE+DTLS to complete before
	// sending media. RTCP handling (PLI/FIR) runs independently via TrackSender.
	go whepFeedLoop(stream, videoSender, audioSender, sess.done, connected, mode, info.VideoCodec, targetAudioCodec, bwe)

	m.server.GetEventBus().Emit(core.EventSubscribe, &core.EventContext{
		StreamKey:  streamKey,
		Protocol:   "webrtc",
		RemoteAddr: r.RemoteAddr,
	})

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/webrtc/session/"+sessionID)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(pc.LocalDescription().SDP))

	slog.Info("WHEP session started", "module", "webrtc", "session", sessionID, "stream", streamKey)
}

// normalizeH264Offer adds packetization-mode=1 to H264 fmtp attributes that
// don't specify it. Some WebRTC clients (e.g., GStreamer) omit
// packetization-mode, but pion's codec matcher requires it on both sides.
func normalizeH264Offer(offerSDP string) string {
	var parsed sdp.SessionDescription
	if err := parsed.UnmarshalString(offerSDP); err != nil {
		return offerSDP
	}

	for _, md := range parsed.MediaDescriptions {
		if md.MediaName.Media != "video" {
			continue
		}

		// Collect H264 payload types from rtpmap attributes.
		h264PTs := map[string]bool{}
		for _, attr := range md.Attributes {
			if attr.Key == "rtpmap" {
				parts := strings.SplitN(attr.Value, " ", 2)
				if len(parts) == 2 && strings.HasPrefix(strings.ToUpper(parts[1]), "H264/") {
					h264PTs[parts[0]] = true
				}
			}
		}

		// Append packetization-mode=1 to H264 fmtp lines that lack it.
		for i, attr := range md.Attributes {
			if attr.Key != "fmtp" {
				continue
			}
			parts := strings.SplitN(attr.Value, " ", 2)
			if len(parts) != 2 || !h264PTs[parts[0]] {
				continue
			}
			if !strings.Contains(parts[1], "packetization-mode") {
				md.Attributes[i] = sdp.Attribute{
					Key:   "fmtp",
					Value: parts[0] + " " + parts[1] + ";packetization-mode=1",
				}
			}
		}
	}

	result, err := parsed.Marshal()
	if err != nil {
		return offerSDP
	}
	return string(result)
}

