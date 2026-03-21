package webrtc

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

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
			log.Printf("[webrtc] WHIP set publisher failed: %v", err)
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
			log.Printf("[webrtc] WHIP unsupported codec: %s", codec.MimeType)
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
			log.Printf("[webrtc] WHIP depacketizer error: %v", err)
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

	gatherComplete := webrtc.GatheringCompletePromise(pc)

	if err := pc.SetLocalDescription(answer); err != nil {
		sess.Close()
		releaseConn()
		http.Error(w, fmt.Sprintf("set local description: %v", err), http.StatusInternalServerError)
		return
	}

	<-gatherComplete

	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/webrtc/session/"+sessionID)
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(pc.LocalDescription().SDP))

	log.Printf("[webrtc] WHIP session %s started for stream %s", sessionID, streamKey)
}

// readTrackLoop reads RTP packets from a WebRTC track, depacketizes them,
// and writes AVFrames to the stream.
func readTrackLoop(track *webrtc.TrackRemote, dp pkgrtp.Depacketizer, stream *core.Stream, _ <-chan struct{}, codec avframe.CodecType) {
	var (
		accPayload []byte
		accFrame   avframe.FrameType
		accMedia   avframe.MediaType
		tsBase     uint32
		tsBaseSet  bool
	)

	if codec.IsVideo() {
		accMedia = avframe.MediaTypeVideo
	} else {
		accMedia = avframe.MediaTypeAudio
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
			accPayload = append(accPayload, frame.Payload...)
			if frame.FrameType == avframe.FrameTypeSequenceHeader {
				accFrame = avframe.FrameTypeSequenceHeader
			} else if frame.FrameType == avframe.FrameTypeKeyframe && accFrame != avframe.FrameTypeSequenceHeader {
				accFrame = avframe.FrameTypeKeyframe
			} else if accFrame == 0 {
				accFrame = frame.FrameType
			}
		}

		if pkt.Marker && len(accPayload) > 0 {
			if !tsBaseSet {
				tsBase = pkt.Timestamp
				tsBaseSet = true
			}
			clockRate := uint32(90000)
			if codec.IsAudio() {
				clockRate = uint32(track.Codec().ClockRate)
			}
			dts := int64(pkt.Timestamp-tsBase) * 1000 / int64(clockRate)

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
