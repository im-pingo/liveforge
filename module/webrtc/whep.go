package webrtc

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

type whepTrackFactory func(*webrtc.PeerConnection, webrtc.RTPCodecCapability, string, string, string) (*TrackSender, error)

// handleWHEP handles POST /webrtc/whep/{path...} for WHEP playback.
func (m *Module) handleWHEP(w http.ResponseWriter, r *http.Request) {
	if !m.beginSetup() {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	defer m.endSetup()

	streamKey := r.PathValue("path")
	if streamKey == "" {
		http.Error(w, "missing stream key", http.StatusBadRequest)
		return
	}
	subscribeCtx := eventContextFromRequest(r, streamKey)
	if err := m.server.Authorize(r.Context(), authorizationRequestFromEvent(
		core.AuthorizationSubscribe,
		core.AuthorizationPreSession,
		subscribeCtx,
	)); err != nil {
		rejectUnauthorized(w)
		return
	}

	if !m.server.AcquireConn() {
		http.Error(w, "max connections reached", http.StatusServiceUnavailable)
		return
	}
	var releaseConnOnce sync.Once
	releaseConn := func() {
		releaseConnOnce.Do(m.server.ReleaseConn)
	}

	contentType := r.Header.Get("Content-Type")
	if !strings.Contains(contentType, "application/sdp") {
		releaseConn()
		http.Error(w, "Content-Type must be application/sdp", http.StatusUnsupportedMediaType)
		return
	}

	offerBytes, ok := readSDPOffer(w, r)
	if !ok {
		releaseConn()
		return
	}
	if m.isClosing() {
		releaseConn()
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
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

	pending := stream.StartupSnapshot()
	if pending.Generation == 0 || !stream.IsPublisherGeneration(pending.Generation) {
		releaseConn()
		http.Error(w, "stream startup unavailable", http.StatusNotFound)
		return
	}
	startup, ready := waitWHEPStartup(r.Context(), stream, pending)
	if !ready {
		releaseConn()
		if r.Context().Err() != nil {
			http.Error(w, "WHEP startup canceled", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "stream startup unavailable", http.StatusNotFound)
		}
		return
	}
	info := &startup.MediaInfo

	// Track subscriber limit.
	releaseSubscriber, err := stream.AddSubscriberForGeneration("webrtc", startup.Generation)
	if err != nil {
		releaseConn()
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	// Set GCC initial bitrate to the stream's actual bitrate (with 20% headroom)
	// so the pacer doesn't throttle at startup.
	var initialBitrate int64
	if stats := stream.Stats(); stats.BitrateKbps > 0 {
		initialBitrate = stats.BitrateKbps * 1000 * 120 / 100 // kbps→bps + 20%
	}

	pc, rtpStats, bwe, err := m.newPeerConnection(webrtc.Configuration{
		ICEServers: m.iceServersFromConfig(),
	}, initialBitrate)
	if err != nil {
		releaseSubscriber()
		releaseConn()
		http.Error(w, "failed to create peer connection", http.StatusInternalServerError)
		return
	}

	sessionID := uuid.New().String()
	sess := newSession(sessionID, pc, streamKey, "whep", m)
	lifecycleCtx := *subscribeCtx
	lifecycleCtx.SubscriberID = sessionID
	lifecycleCtx.StreamInstanceID = startup.StreamInstanceID
	lifecycleCtx.PublisherGeneration = startup.Generation
	lifecycleCtx.PublisherID = startup.PublisherID
	sess.setCleanup(func() {
		releaseSubscriber()
		sess.stopLifecycle(m.server.GetEventBus(), core.EventSubscribeStop, &lifecycleCtx)
		releaseConn()
	})
	if !m.storeSession(sess) {
		sess.Close()
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}

	// Parse the offer SDP to determine which media types the client requests.
	// Only add tracks that match an m-line in the offer; adding tracks without
	// a corresponding offer m-line causes pion's SetLocalDescription to fail
	// with "codec is not supported by remote".
	var offerHasVideo, offerHasAudio bool
	var parsedSDP sdp.SessionDescription
	if err := parsedSDP.UnmarshalString(offer.SDP); err == nil {
		offerHasVideo = offerRequestsMedia(&parsedSDP, "video")
		offerHasAudio = offerRequestsMedia(&parsedSDP, "audio")
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
	var targetAudioCodec avframe.CodecType
	var audioNeedsTranscode bool

	if info.HasVideo() && offerHasVideo {
		mime := codecToMime(info.VideoCodec)
		if mime == "" || !offerSupportsCodec(&parsedSDP, "video", mime) {
			slog.Debug("WHEP video codec mismatch", "module", "webrtc",
				"publisher", mime, "stream", streamKey)
			sess.Close()
			http.Error(w, "source video codec is not supported by the offer", http.StatusUnsupportedMediaType)
			return
		}
		videoSender, err = m.createWHEPTrackSender(pc,
			webrtc.RTPCodecCapability{MimeType: mime, ClockRate: 90000},
			sessionID, sessionID+"-video", "liveforge",
		)
		if err != nil {
			sess.Close()
			http.Error(w, "failed to create WHEP video track", http.StatusInternalServerError)
			return
		}
	}

	if info.HasAudio() && offerHasAudio {
		tm := stream.TranscodeManager()
		targetAudioCodec, audioNeedsTranscode = selectWHEPAudioCodecForOffer(&parsedSDP, info.AudioCodec, func(target avframe.CodecType) bool {
			return tm != nil && tm.CanTranscode(info.AudioCodec, target)
		})
		mime := codecToMime(targetAudioCodec)
		if mime == "" {
			sess.Close()
			http.Error(w, "source audio codec is not supported by the offer", http.StatusUnsupportedMediaType)
			return
		}
		clockRate := uint32(48000)
		channels := uint16(2)
		// The target codec, rather than source metadata, determines the RTP
		// clock/channel format for G.711. This also keeps direct PCMA/PCMU
		// passthrough valid when an upstream publisher omitted its parameters.
		if targetAudioCodec == avframe.CodecG711U || targetAudioCodec == avframe.CodecG711A {
			clockRate = 8000
			channels = 1
		} else if !audioNeedsTranscode {
			// For direct codec passthrough, use the publisher's parameters.
			if info.SampleRate > 0 {
				if info.SampleRate > 1<<32-1 {
					sess.Close()
					http.Error(w, "source audio sample rate is out of range", http.StatusUnsupportedMediaType)
					return
				}
				clockRate = uint32(info.SampleRate) // #nosec G115 -- range checked against RTP's uint32 clock rate.
			}
			if info.Channels > 0 {
				if info.Channels > 1<<16-1 {
					sess.Close()
					http.Error(w, "source audio channel count is out of range", http.StatusUnsupportedMediaType)
					return
				}
				channels = uint16(info.Channels) // #nosec G115 -- range checked against RTP's uint16 channel count.
			}
		}
		audioSender, err = m.createWHEPTrackSender(pc,
			webrtc.RTPCodecCapability{MimeType: mime, ClockRate: clockRate, Channels: channels},
			sessionID, sessionID+"-audio", "liveforge",
		)
		if err != nil {
			sess.Close()
			http.Error(w, "failed to create WHEP audio track", http.StatusInternalServerError)
			return
		}
	}

	if videoSender == nil && audioSender == nil {
		sess.Close()
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

	// Signal channel for when ICE and DTLS/SRTP are ready to send media. ICE
	// can reach "connected" before the DTLS handshake completes; sending the
	// cached GOP at that point loses the only keyframe a late WHEP viewer may
	// receive. Feed startup is therefore gated by PeerConnectionStateConnected.
	connected := make(chan struct{})
	var connectedOnce sync.Once
	markConnected := func() {
		connectedOnce.Do(func() { close(connected) })
	}

	// Merge ICE state handling: log state, signal connection ready, and
	// clean up on disconnect. This replaces the handler set by newSession.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		slog.Debug("WHEP ICE state", "module", "webrtc", "session", sessionID, "state", state)
		switch state {
		case webrtc.ICEConnectionStateFailed, webrtc.ICEConnectionStateClosed:
			sess.Close()
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		slog.Debug("WHEP peer connection state", "module", "webrtc", "session", sessionID, "state", state)
		switch state {
		case webrtc.PeerConnectionStateConnected:
			markConnected()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			sess.Close()
		}
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		sess.Close()
		http.Error(w, fmt.Sprintf("set remote description: %v", err), http.StatusBadRequest)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		sess.Close()
		http.Error(w, fmt.Sprintf("create answer: %v", err), http.StatusInternalServerError)
		return
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		sess.Close()
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

	// "live" (default): send GOP cache like other protocols for immediate
	// playback. "realtime" is explicit low-latency mode and waits for the next
	// live keyframe when the snapshot cannot provide one.
	mode := normalizeWHEPMode(r.URL.Query().Get("mode"))

	// Start the feed goroutine. It waits for ICE+DTLS to complete before
	// sending media. RTCP handling (PLI/FIR) runs independently via TrackSender.
	feedStatus := newWHEPFeedStatus(startup.Generation, startup.LiveCursor, mode)
	feedStatus.setExpectedMedia(videoSender != nil, audioSender != nil)
	sess.setFeedStatus(feedStatus)
	sess.setFeedTracks(videoSender, audioSender, rtpStats)
	if !sess.startLifecycle(m.server.GetEventBus(), core.EventSubscribe, &lifecycleCtx) {
		sess.Close()
		http.Error(w, "session closed during setup", http.StatusServiceUnavailable)
		return
	}
	go func() {
		whepFeedLoop(stream, startup, videoSender, audioSender, sess.done, connected, mode, targetAudioCodec, bwe, feedStatus, sess.sendGate)
		sess.Close()
	}()

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/webrtc/session/"+sessionID)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(pc.LocalDescription().SDP))

	slog.Info("WHEP session started", "module", "webrtc", "session", sessionID, "stream", streamKey)
}

func (m *Module) createWHEPTrackSender(pc *webrtc.PeerConnection, capability webrtc.RTPCodecCapability, sessionID, trackID, streamID string) (*TrackSender, error) {
	if m.whepTrackFactory != nil {
		return m.whepTrackFactory(pc, capability, sessionID, trackID, streamID)
	}
	track, err := webrtc.NewTrackLocalStaticSample(capability, trackID, streamID)
	if err != nil {
		return nil, fmt.Errorf("create local track: %w", err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return nil, fmt.Errorf("add local track: %w", err)
	}
	return NewTrackSender(sessionID, track, sender), nil
}

func normalizeWHEPMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "realtime":
		return "realtime"
	case "live":
		return "live"
	default:
		return "live"
	}
}

func waitWHEPStartup(ctx context.Context, stream *core.Stream, pending core.StreamStartupSnapshot) (core.StreamStartupSnapshot, bool) {
	if pending.Generation == 0 || pending.GenerationDone == nil || pending.PublisherID == "" {
		return core.StreamStartupSnapshot{}, false
	}
	if pending.Ready {
		return pending, stream.IsPublisherGeneration(pending.Generation)
	}

	waitCtx, cancel := context.WithCancel(ctx)
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-pending.GenerationDone:
			cancel()
		case <-waitCtx.Done():
		}
	}()
	snapshot, ok := stream.WaitForStartup(waitCtx)
	cancel()
	<-watcherDone
	if !ok || snapshot.Generation != pending.Generation || snapshot.PublisherID != pending.PublisherID {
		return core.StreamStartupSnapshot{}, false
	}
	return snapshot, stream.IsPublisherGeneration(snapshot.Generation)
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

// offerSupportsCodec checks whether the remote offer contains the given codec
// MIME type (e.g., "video/H265") in its rtpmap attributes.
func offerSupportsCodec(parsed *sdp.SessionDescription, media, mime string) bool {
	codecName := strings.ToUpper(mime)
	if idx := strings.LastIndex(codecName, "/"); idx >= 0 {
		codecName = codecName[idx+1:]
	}
	for _, md := range parsed.MediaDescriptions {
		if md.MediaName.Media != media || !mediaDescriptionRequestsReceive(parsed, md) {
			continue
		}
		offeredPayloads := make(map[string]struct{}, len(md.MediaName.Formats))
		for _, payload := range md.MediaName.Formats {
			offeredPayloads[payload] = struct{}{}
		}
		for _, attr := range md.Attributes {
			if attr.Key != "rtpmap" {
				continue
			}
			fields := strings.Fields(attr.Value)
			if len(fields) != 2 {
				continue
			}
			if _, ok := offeredPayloads[fields[0]]; !ok {
				continue
			}
			encoding := strings.SplitN(fields[1], "/", 2)[0]
			if strings.EqualFold(encoding, codecName) {
				return true
			}
		}
	}
	return false
}

func offerRequestsMedia(parsed *sdp.SessionDescription, media string) bool {
	for _, description := range parsed.MediaDescriptions {
		if description.MediaName.Media == media && mediaDescriptionRequestsReceive(parsed, description) {
			return true
		}
	}
	return false
}

func mediaDescriptionRequestsReceive(parsed *sdp.SessionDescription, description *sdp.MediaDescription) bool {
	if description.MediaName.Port.Value == 0 {
		return false
	}
	if receives, specified := directionRequestsReceive(description.Attributes); specified {
		return receives
	}
	if receives, specified := directionRequestsReceive(parsed.Attributes); specified {
		return receives
	}
	return true
}

func directionRequestsReceive(attributes []sdp.Attribute) (receives, specified bool) {
	for _, attribute := range attributes {
		switch strings.ToLower(attribute.Key) {
		case "inactive", "sendonly":
			return false, true
		case "recvonly", "sendrecv":
			return true, true
		}
	}
	return false, false
}

func selectWHEPAudioCodec(parsed *sdp.SessionDescription, source avframe.CodecType, canTranscode bool) (avframe.CodecType, bool) {
	if mime := codecToMime(source); mime != "" && offerSupportsCodec(parsed, "audio", mime) {
		return source, false
	}
	return selectWHEPAudioCodecForOffer(parsed, source, func(target avframe.CodecType) bool {
		return canTranscode
	})
}

// selectWHEPAudioCodecForOffer chooses a codec the offer can receive. Opus is
// preferred, but PCMU/PCMA are valid browser fallbacks when the offer does not
// advertise Opus. The capability callback is target-specific so a configured
// registry cannot be mistaken for support for every conversion pair.
func selectWHEPAudioCodecForOffer(parsed *sdp.SessionDescription, source avframe.CodecType, canTranscode func(avframe.CodecType) bool) (avframe.CodecType, bool) {
	if source == 0 {
		return 0, false
	}
	if mime := codecToMime(source); mime != "" && offerSupportsCodec(parsed, "audio", mime) {
		return source, false
	}
	if canTranscode == nil {
		return 0, false
	}
	for _, target := range []struct {
		codec avframe.CodecType
		mime  string
	}{
		{avframe.CodecOpus, webrtc.MimeTypeOpus},
		{avframe.CodecG711U, webrtc.MimeTypePCMU},
		{avframe.CodecG711A, webrtc.MimeTypePCMA},
	} {
		if target.codec == source || !canTranscode(target.codec) {
			continue
		}
		if offerSupportsCodec(parsed, "audio", target.mime) {
			return target.codec, true
		}
	}
	return 0, false
}
