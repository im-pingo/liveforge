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
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	pionrtp "github.com/pion/rtp/v2"
	"github.com/pion/webrtc/v4"
)

// handleWHIP handles POST /webrtc/whip/{path...} for WHIP publish.
func (m *Module) handleWHIP(w http.ResponseWriter, r *http.Request) {
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
		info: &avframe.MediaInfo{},
		pc:   pc,
		done: make(chan struct{}),
	}

	sess := newSession(sessionID, pc, streamKey, "whip", m)
	m.storeSession(sess)

	var (
		videoDetected bool
		audioDetected bool
		publisherSet  bool
		pubMu         sync.Mutex
	)

	setPublisherOnce := func() {
		pubMu.Lock()
		defer pubMu.Unlock()
		if publisherSet || (!videoDetected && !audioDetected) {
			return
		}
		if err := stream.SetPublisher(pub); err != nil {
			slog.Error("WHIP set publisher failed", "module", "webrtc", "error", err)
			return
		}
		publisherSet = true
		m.server.GetEventBus().Emit(core.EventPublish, &core.EventContext{
			StreamKey:  streamKey,
			Protocol:   "webrtc",
			RemoteAddr: r.RemoteAddr,
		})
	}

	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		codec := track.Codec()
		avCodec := mimeToCodecType(codec.MimeType)
		if avCodec == 0 {
			slog.Warn("WHIP unsupported codec", "module", "webrtc", "mime", codec.MimeType)
			return
		}

		pubMu.Lock()
		if avCodec.IsVideo() {
			pub.info.VideoCodec = avCodec
			videoDetected = true
		} else {
			pub.info.AudioCodec = avCodec
			pub.info.SampleRate = int(codec.ClockRate)
			pub.info.Channels = int(codec.Channels)
			audioDetected = true
		}
		pubMu.Unlock()

		setPublisherOnce()

		dp, err := pkgrtp.NewDepacketizer(avCodec)
		if err != nil {
			slog.Error("WHIP depacketizer error", "module", "webrtc", "error", err)
			return
		}

		readTrackLoop(track, dp, stream, pub.done, avCodec)
	})

	// Cleanup on ICE disconnect.
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateClosed {
			pubMu.Lock()
			wasPublisher := publisherSet
			pubMu.Unlock()

			if wasPublisher {
				stream.RemovePublisher()
				m.server.GetEventBus().Emit(core.EventPublishStop, &core.EventContext{
					StreamKey:  streamKey,
					Protocol:   "webrtc",
					RemoteAddr: r.RemoteAddr,
				})
			}
			releaseConn()
		}
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		sess.Close()
		releaseConn()
		http.Error(w, fmt.Sprintf("set remote description: %v", err), http.StatusBadRequest)
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		sess.Close()
		releaseConn()
		http.Error(w, fmt.Sprintf("create answer: %v", err), http.StatusInternalServerError)
		return
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		sess.Close()
		releaseConn()
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

// readTrackLoop reads RTP packets from a WebRTC track, depacketizes them,
// and writes AVFrames to the stream.
//
// Key invariant for H.264: SPS/PPS arrive as SequenceHeader frames and may
// be interleaved with IDR NALs in the same Marker-delimited window. We handle
// them as separate AVFrames so the ring buffer and GOP cache stay consistent:
//   - SequenceHeader (SPS/PPS): flushed immediately, resets accSeqHeader payload.
//   - Keyframe/Interframe: accumulated and flushed on the Marker bit.
func readTrackLoop(track *webrtc.TrackRemote, dp pkgrtp.Depacketizer, stream *core.Stream, _ <-chan struct{}, codec avframe.CodecType) {
	var (
		accPayload    []byte
		accFrame      avframe.FrameType
		accMedia      avframe.MediaType
		accSeqPayload []byte // accumulated SPS/PPS to write as SequenceHeader
		tsBase        uint32
		tsBaseSet     bool
	)

	if codec.IsVideo() {
		accMedia = avframe.MediaTypeVideo
	} else {
		accMedia = avframe.MediaTypeAudio
	}

	computeDTS := func(ts uint32) int64 {
		if !tsBaseSet {
			tsBase = ts
			tsBaseSet = true
		}
		clockRate := int64(90000)
		if codec.IsAudio() {
			clockRate = int64(track.Codec().ClockRate)
		}
		return int64(ts-tsBase) * 1000 / clockRate
	}

	buf := make([]byte, 1500)
	for {
		n, _, readErr := track.Read(buf)
		if readErr != nil {
			return
		}

		// Parse raw bytes into pion/rtp/v2 Packet (our depacketizers' expected type).
		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		frame, err := dp.Depacketize(&pkt)
		if err != nil {
			continue
		}

		if frame != nil {
			if frame.FrameType == avframe.FrameTypeSequenceHeader {
				// SPS/PPS: accumulate separately. If there is already pending
				// media data (unlikely but possible), flush it first.
				if len(accPayload) > 0 {
					dts := computeDTS(pkt.Timestamp)
					avF := avframe.NewAVFrame(accMedia, codec, accFrame, dts, dts, accPayload)
					stream.WriteFrame(avF)
					accPayload = nil
					accFrame = 0
				}
				accSeqPayload = append(accSeqPayload, frame.Payload...)
			} else {
				// Media frame: if we have buffered SPS/PPS, flush them now as
				// SequenceHeader before the IDR so the ring buffer sees the
				// parameter sets first.
				if len(accSeqPayload) > 0 {
					dts := computeDTS(pkt.Timestamp)
					seqF := avframe.NewAVFrame(accMedia, codec, avframe.FrameTypeSequenceHeader, dts, dts, accSeqPayload)
					stream.WriteFrame(seqF)
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
			dts := computeDTS(pkt.Timestamp)
			avF := avframe.NewAVFrame(accMedia, codec, accFrame, dts, dts, accPayload)
			stream.WriteFrame(avF)
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
type WHIPPublisher struct {
	id   string
	info *avframe.MediaInfo
	pc   *webrtc.PeerConnection
	done chan struct{}
}

var _ core.Publisher = (*WHIPPublisher)(nil)

func (p *WHIPPublisher) ID() string                    { return p.id }
func (p *WHIPPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *WHIPPublisher) Close() error {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
	return p.pc.Close()
}
