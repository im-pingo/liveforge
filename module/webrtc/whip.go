package webrtc

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/pion/rtcp"
	pionrtp "github.com/pion/rtp/v2"
	"github.com/pion/webrtc/v4"
)

// handleWHIP handles POST /webrtc/whip/{path...} for WHIP publish.
func (m *Module) handleWHIP(w http.ResponseWriter, r *http.Request) {
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
	publishCtx := eventContextFromRequest(r, streamKey)
	if err := m.server.Authorize(r.Context(), authorizationRequestFromEvent(
		core.AuthorizationPublish,
		core.AuthorizationPreSession,
		publishCtx,
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

	pc, err := m.api.NewPeerConnection(webrtc.Configuration{
		ICEServers: m.iceServersFromConfig(),
	})
	if err != nil {
		releaseConn()
		http.Error(w, "failed to create peer connection", http.StatusInternalServerError)
		return
	}

	sessionID := uuid.New().String()

	stream, err := m.server.StreamHub().GetOrCreate(streamKey)
	if err != nil {
		pc.Close()
		releaseConn()
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	pub := &WHIPPublisher{
		id:   sessionID,
		pc:   pc,
		done: make(chan struct{}),
	}
	pub.info.Store(&avframe.MediaInfo{})

	sess := newSession(sessionID, pc, streamKey, "whip", m)

	var (
		videoDetected bool
		audioDetected bool
		publisherSet  bool
		pubMu         sync.Mutex
	)
	mediaClock := newWHIPMediaClock()
	sess.setCleanup(func() {
		pubMu.Lock()
		wasPublisher := publisherSet
		pubMu.Unlock()
		if wasPublisher {
			stream.RemovePublisherIf(pub)
		}
		lifecycleCtx := *publishCtx
		lifecycleCtx.PublisherID = pub.ID()
		sess.stopLifecycle(m.server.GetEventBus(), core.EventPublishStop, &lifecycleCtx)
		releaseConn()
	})
	if !m.storeSession(sess) {
		sess.Close()
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}

	setPublisherOnce := func() {
		pubMu.Lock()
		defer pubMu.Unlock()
		if publisherSet || (!videoDetected && !audioDetected) || sess.isClosed() {
			return
		}
		if err := stream.SetPublisher(pub); err != nil {
			slog.Error("WHIP set publisher failed", "module", "webrtc", "error", err)
			return
		}
		publisherSet = true
		lifecycleCtx := *publishCtx
		lifecycleCtx.PublisherID = pub.ID()
		sess.startLifecycle(m.server.GetEventBus(), core.EventPublish, &lifecycleCtx)
	}

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		codec := track.Codec()
		avCodec := mimeToCodecType(codec.MimeType)
		if avCodec == 0 {
			slog.Warn("WHIP unsupported codec", "module", "webrtc", "mime", codec.MimeType)
			return
		}

		pubMu.Lock()
		// Copy-on-write under pubMu (serializes concurrent OnTrack callbacks);
		// readers load the snapshot atomically without the lock.
		mi := *pub.info.Load()
		if avCodec.IsVideo() {
			mi.VideoCodec = avCodec
			videoDetected = true
		} else {
			mi.AudioCodec = avCodec
			mi.SampleRate = int(codec.ClockRate)
			mi.Channels = int(codec.Channels)
			audioDetected = true
		}
		pub.info.Store(&mi)
		pubMu.Unlock()

		setPublisherOnce()

		dp, err := pkgrtp.NewDepacketizer(avCodec)
		if err != nil {
			slog.Error("WHIP depacketizer error", "module", "webrtc", "error", err)
			return
		}
		if avCodec.IsVideo() {
			go requestWHIPKeyframes(pc, uint32(track.SSRC()), sess.done, 2*time.Second)
		}

		readTrackLoop(track, dp, stream, pub, pub.done, avCodec, mediaClock)
	})

	// Cleanup on ICE disconnect.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateClosed {
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
	// whichever comes first. Avoids blocking on slow STUN/TURN timeouts.
	gatherDone := make(chan struct{})
	var gatherOnce sync.Once
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
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

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/webrtc/session/"+sessionID)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(pc.LocalDescription().SDP))

	slog.Info("WHIP session started", "module", "webrtc", "session", sessionID, "stream", streamKey)
}

func requestWHIPKeyframes(pc *webrtc.PeerConnection, mediaSSRC uint32, done <-chan struct{}, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	request := func() bool {
		if err := pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: mediaSSRC}}); err != nil {
			return false
		}
		return true
	}
	if !request() {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if !request() {
				return
			}
		}
	}
}

// readTrackLoop reads RTP packets from a WebRTC track, depacketizes them,
// and writes AVFrames to the stream.
//
// Key invariant for H.264/H.265: parameter sets may be interleaved with IDR
// NALs in the same Marker-delimited window. We handle them as separate
// AVFrames so the ring buffer and GOP cache stay consistent:
//   - SequenceHeader (SPS/PPS): flushed immediately, resets accSeqHeader payload.
//   - Keyframe/Interframe: accumulated and flushed on the Marker bit.
func readTrackLoop(track *webrtc.TrackRemote, dp pkgrtp.Depacketizer, stream *core.Stream, pub *WHIPPublisher, _ <-chan struct{}, codec avframe.CodecType, mediaClock *whipMediaClock) {
	var (
		accPayload    []byte
		accFrame      avframe.FrameType
		accMedia      avframe.MediaType
		accSeqPayload []byte // accumulated SPS/PPS to write as SequenceHeader
	)

	if codec.IsVideo() {
		accMedia = avframe.MediaTypeVideo
	} else {
		accMedia = avframe.MediaTypeAudio
	}

	clockRate := int64(90000)
	if codec.IsAudio() {
		clockRate = int64(track.Codec().ClockRate)
	}
	if mediaClock == nil {
		mediaClock = newWHIPMediaClock()
	}
	writeFrame := func(frame *avframe.AVFrame) bool {
		if stream.WriteFrameForPublisher(pub, frame) {
			return true
		}
		if stream.Publisher() == pub {
			return true
		}
		_ = pub.Close()
		return false
	}

	buf := make([]byte, 1500)
	for {
		n, _, readErr := track.Read(buf)
		if readErr != nil {
			return
		}
		packetArrival := time.Now()

		// Parse raw bytes into pion/rtp/v2 Packet (our depacketizers' expected type).
		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		frames, err := pkgrtp.DepacketizeFrames(dp, &pkt)
		if err != nil {
			continue
		}

		for _, frame := range frames {
			if frame == nil {
				continue
			}
			if !codec.IsVideo() {
				dts := mediaClock.DTS(uint32(track.SSRC()), pkt.Timestamp, clockRate, packetArrival)
				frameType := frame.FrameType
				if frameType == 0 {
					frameType = avframe.FrameTypeInterframe
				}
				if !writeFrame(avframe.NewAVFrame(
					avframe.MediaTypeAudio,
					codec,
					frameType,
					dts,
					dts,
					frame.Payload,
				)) {
					return
				}
				continue
			}
			if frame.FrameType == avframe.FrameTypeSequenceHeader {
				// SPS/PPS: accumulate separately. If there is already pending
				// media data (unlikely but possible), flush it first.
				if len(accPayload) > 0 {
					dts := mediaClock.DTS(uint32(track.SSRC()), pkt.Timestamp, clockRate, packetArrival)
					avF := avframe.NewAVFrame(accMedia, codec, accFrame, dts, dts, accPayload)
					if !writeFrame(avF) {
						return
					}
					accPayload = nil
					accFrame = 0
				}
				accSeqPayload = append(accSeqPayload[:0], frame.Payload...)
			} else {
				// Media frame: if we have buffered SPS/PPS, flush them now as
				// SequenceHeader before the IDR so the ring buffer sees the
				// parameter sets first.
				if len(accSeqPayload) > 0 {
					dts := mediaClock.DTS(uint32(track.SSRC()), pkt.Timestamp, clockRate, packetArrival)
					seqF := avframe.NewAVFrame(accMedia, codec, avframe.FrameTypeSequenceHeader, dts, dts, accSeqPayload)
					if !writeFrame(seqF) {
						return
					}
					accSeqPayload = nil
				}
				accPayload = append(accPayload, frame.Payload...)
				if frame.FrameType == avframe.FrameTypeKeyframe {
					accFrame = avframe.FrameTypeKeyframe
				} else if accFrame == 0 {
					accFrame = frame.FrameType
				}
			}
		}

		// Flush accumulated media payload on the Marker bit (end of access unit).
		if pkt.Marker && len(accPayload) > 0 {
			dts := mediaClock.DTS(uint32(track.SSRC()), pkt.Timestamp, clockRate, packetArrival)
			avF := avframe.NewAVFrame(accMedia, codec, accFrame, dts, dts, accPayload)
			if !writeFrame(avF) {
				return
			}
			accPayload = nil
			accFrame = 0
		}
	}
}

// mimeToCodecType maps WebRTC MIME types to avframe CodecType.
func mimeToCodecType(mime string) avframe.CodecType {
	mime = strings.ToLower(mime)
	switch {
	case strings.Contains(mime, "h264"):
		return avframe.CodecH264
	case strings.Contains(mime, "h265") || strings.Contains(mime, "hevc"):
		return avframe.CodecH265
	case strings.Contains(mime, "vp8"):
		return avframe.CodecVP8
	case strings.Contains(mime, "vp9"):
		return avframe.CodecVP9
	case strings.Contains(mime, "av1"):
		return avframe.CodecAV1
	case strings.Contains(mime, "opus"):
		return avframe.CodecOpus
	case strings.Contains(mime, "pcmu"):
		return avframe.CodecG711U
	case strings.Contains(mime, "pcma"):
		return avframe.CodecG711A
	default:
		return 0
	}
}

// WHIPPublisher implements core.Publisher for WebRTC WHIP ingest.
//
// MediaInfo is stored behind an atomic pointer: OnTrack callbacks publish
// updated snapshots while subscriber goroutines read concurrently.
type WHIPPublisher struct {
	id   string
	info atomic.Pointer[avframe.MediaInfo]
	pc   *webrtc.PeerConnection
	done chan struct{}
}

var _ core.Publisher = (*WHIPPublisher)(nil)

func (p *WHIPPublisher) ID() string                    { return p.id }
func (p *WHIPPublisher) MediaInfo() *avframe.MediaInfo { return p.info.Load() }
func (p *WHIPPublisher) Close() error {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
	return p.pc.Close()
}
