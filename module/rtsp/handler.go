package rtsp

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/portalloc"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

// Handler processes RTSP requests.
type Handler struct {
	server                 *core.Server
	ports                  *portalloc.PortAllocator
	multicast              *config.MulticastConfig // nil if multicast disabled
	writeFrameForPublisher func(*core.Stream, core.Publisher, *avframe.AVFrame) bool
}

// NewHandler creates a new RTSP handler.
func NewHandler(server *core.Server, ports *portalloc.PortAllocator, multicast *config.MulticastConfig) *Handler {
	return &Handler{
		server:    server,
		ports:     ports,
		multicast: multicast,
		writeFrameForPublisher: func(stream *core.Stream, pub core.Publisher, frame *avframe.AVFrame) bool {
			return stream.WriteFrameForPublisher(pub, frame)
		},
	}
}

// newResponse creates a base response with CSeq from request.
func newResponse(statusCode int, reason string, req *Request) *Response {
	resp := &Response{
		StatusCode: statusCode,
		Reason:     reason,
		Headers:    make(http.Header),
	}
	if req != nil {
		resp.Headers.Set("CSeq", req.Headers.Get("CSeq"))
	}
	return resp
}

// HandleOptions responds with supported methods.
func (h *Handler) HandleOptions(req *Request) *Response {
	resp := newResponse(200, "OK", req)
	resp.Headers.Set("Public", "OPTIONS, DESCRIBE, SETUP, PLAY, PAUSE, ANNOUNCE, RECORD, TEARDOWN, GET_PARAMETER")
	return resp
}

// HandleGetParameter serves as a keepalive.
func (h *Handler) HandleGetParameter(req *Request) *Response {
	return newResponse(200, "OK", req)
}

// HandleDescribe returns SDP for the stream.
func (h *Handler) HandleDescribe(req *Request, session *RTSPSession) *Response {
	// Extract stream key from URL (e.g., rtsp://host/live/test -> live/test)
	streamKey := extractStreamKey(req.URL)
	if h.server == nil {
		resp := newResponse(200, "OK", req)
		resp.Headers.Set("Content-Type", "application/sdp")
		return resp
	}
	stream, ok := h.server.StreamHub().Find(streamKey)
	if !ok {
		return newResponse(404, "Stream Not Found", req)
	}
	startup := stream.StartupSnapshot()
	if !startup.Ready || !stream.IsPublisherGeneration(startup.Generation) {
		return newResponse(404, "Stream Not Found", req)
	}
	sd := sdp.BuildFromMediaInfo(&startup.MediaInfo, req.URL, "0.0.0.0")
	body := sd.Marshal()
	slog.Debug("DESCRIBE SDP", "module", "rtsp", "body", string(body))
	resp := newResponse(200, "OK", req)
	resp.Headers.Set("Content-Type", "application/sdp")
	resp.Headers.Set("Content-Base", req.URL+"/")
	resp.Headers.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	resp.Body = body
	if session != nil {
		if !session.SetDescription(startup, stream) {
			return newResponse(454, "Session Not Found", req)
		}
		if err := session.Transition(StateDescribed); err != nil {
			return newResponse(455, "Method Not Valid in This State", req)
		}
	}
	return resp
}

// TransportConfig holds parsed Transport header data.
type TransportConfig struct {
	IsTCP       bool
	IsMulticast bool
	Interleaved [2]int // channel pair for TCP
	ClientPorts [2]int // client ports for UDP
	ServerPorts [2]int // allocated server ports for UDP
}

// HandleSetup negotiates transport for a track.
func (h *Handler) HandleSetup(req *Request, session *RTSPSession, remoteAddr string) *Response {
	trackID, ok := extractTrackID(req.URL)
	if !ok {
		return newResponse(455, "Method Not Valid in This State", req)
	}
	if session != nil {
		if setupResult := session.validateSetupTrack(trackID); setupResult != trackSetupOK {
			if setupResult == trackSetupSessionClosed {
				return newResponse(454, "Session Not Found", req)
			}
			return newResponse(455, "Method Not Valid in This State", req)
		}
	}

	transport := req.Headers.Get("Transport")
	tc := parseTransportHeader(transport)

	var udpTransport *UDPTransport
	var mcastTransport *MulticastTransport

	if tc.IsMulticast && h.multicast != nil {
		mt, err := NewMulticastTransport(*h.multicast)
		if err != nil {
			return newResponse(500, "Internal Server Error", req)
		}
		rtpPort, rtcpPort := mt.ServerPorts()
		tc.ServerPorts = [2]int{rtpPort, rtcpPort}
		mcastTransport = mt
	} else if tc.IsMulticast {
		return newResponse(461, "Unsupported Transport", req)
	} else if !tc.IsTCP && h.ports != nil {
		ut, err := NewUDPTransport(h.ports)
		if err != nil {
			return newResponse(500, "Internal Server Error", req)
		}
		rtpPort, rtcpPort := ut.ServerPorts()
		tc.ServerPorts = [2]int{rtpPort, rtcpPort}
		udpTransport = ut

		host, _, _ := net.SplitHostPort(remoteAddr)
		clientIP := net.ParseIP(host)
		if clientIP != nil {
			ut.SetClientAddr(clientIP, tc.ClientPorts[0], tc.ClientPorts[1])
		}
	}

	if session != nil {
		ts := TrackSetup{
			TrackID:   trackID,
			Transport: tc,
			UDP:       udpTransport,
			Multicast: mcastTransport,
		}
		setupResult := session.setupTrack(ts)
		if setupResult != trackSetupOK {
			if udpTransport != nil {
				udpTransport.Close()
			}
			if mcastTransport != nil {
				mcastTransport.Close()
			}
			if setupResult == trackSetupSessionClosed {
				return newResponse(454, "Session Not Found", req)
			}
			return newResponse(455, "Method Not Valid in This State", req)
		}
	}

	resp := newResponse(200, "OK", req)
	if tc.IsTCP {
		resp.Headers.Set("Transport", fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", tc.Interleaved[0], tc.Interleaved[1]))
	} else if tc.IsMulticast && mcastTransport != nil {
		resp.Headers.Set("Transport", fmt.Sprintf("RTP/AVP;multicast;destination=%s;port=%d-%d;ttl=%d",
			mcastTransport.MulticastAddr(), tc.ServerPorts[0], tc.ServerPorts[1], h.multicast.TTL))
	} else {
		resp.Headers.Set("Transport", fmt.Sprintf("RTP/AVP;unicast;client_port=%d-%d;server_port=%d-%d",
			tc.ClientPorts[0], tc.ClientPorts[1], tc.ServerPorts[0], tc.ServerPorts[1]))
	}
	if session != nil {
		resp.Headers.Set("Session", session.Snapshot().ID+";timeout=60")
	}
	return resp
}

// HandleAnnounce processes ANNOUNCE request with SDP body.
func (h *Handler) HandleAnnounce(req *Request, session *RTSPSession, remoteAddr string) *Response {
	if session != nil && !session.CanHandleRequest() {
		return newResponse(454, "Session Not Found", req)
	}
	if len(req.Body) == 0 {
		return newResponse(400, "Bad Request", req)
	}
	sd, err := sdp.Parse(req.Body)
	if err != nil {
		return newResponse(400, "Bad Request", req)
	}

	if session != nil && h.server != nil {
		snapshot := session.Snapshot()
		publishCtx := &core.EventContext{
			StreamKey:   snapshot.StreamKey,
			PublisherID: snapshot.ID,
			Protocol:    "rtsp",
			RemoteAddr:  remoteAddr,
		}
		if err := h.server.GetEventBus().EmitSync(core.EventPublish, publishCtx); err != nil {
			return newResponse(401, "Unauthorized", req)
		}

		mediaInfo, ptMap := sdpToMediaInfoWithPT(sd)
		stream, err := h.server.StreamHub().GetOrCreate(snapshot.StreamKey)
		if err != nil {
			return newResponse(503, "Service Unavailable", req)
		}

		pub, err := NewRTSPPublisher(snapshot.ID, mediaInfo, stream, ptMap)
		if err != nil {
			return newResponse(500, "Internal Server Error", req)
		}

		if err := stream.SetPublisher(pub); err != nil {
			_ = pub.Close()
			return newResponse(500, "Internal Server Error", req)
		}
		if !session.SetPublisher(mediaInfo, stream, pub) {
			stream.RemovePublisherIf(pub)
			_ = pub.Close()
			return newResponse(454, "Session Not Found", req)
		}

		// If SPS/PPS were in the SDP (sprop-parameter-sets), feed a synthetic
		// SequenceHeader frame so the stream caches it for late-joining subscribers.
		if len(mediaInfo.VideoSequenceHeader) > 0 {
			seqFrame := avframe.NewAVFrame(
				avframe.MediaTypeVideo,
				avframe.CodecH264,
				avframe.FrameTypeSequenceHeader,
				0, 0,
				mediaInfo.VideoSequenceHeader,
			)
			if !h.writeFrameForPublisher(stream, pub, seqFrame) {
				session.ClearPublisher(pub)
				stream.RemovePublisherIf(pub)
				_ = pub.Close()
				return newResponse(500, "Internal Server Error", req)
			}
			slog.Debug("injected SPS/PPS from SDP", "module", "rtsp", "bytes", len(mediaInfo.VideoSequenceHeader))
		}

		if err := session.Transition(StateAnnounced); err != nil {
			session.ClearPublisher(pub)
			stream.RemovePublisherIf(pub)
			_ = pub.Close()
			if !session.CanHandleRequest() {
				return newResponse(454, "Session Not Found", req)
			}
			return newResponse(455, "Method Not Valid in This State", req)
		}
		session.startPublishLifecycle(func() {
			h.server.GetEventBus().EmitAsync(core.EventPublish, publishCtx)
		})
	}

	if session != nil && h.server == nil {
		if err := session.Transition(StateAnnounced); err != nil {
			if !session.CanHandleRequest() {
				return newResponse(454, "Session Not Found", req)
			}
			return newResponse(455, "Method Not Valid in This State", req)
		}
	}
	return newResponse(200, "OK", req)
}

// HandleRecord starts recording (publishing) on the stream.
func (h *Handler) HandleRecord(req *Request, session *RTSPSession) *Response {
	if session != nil {
		if !session.CanHandleRequest() {
			return newResponse(454, "Session Not Found", req)
		}
		if err := session.Transition(StateRecording); err != nil {
			return newResponse(455, "Method Not Valid in This State", req)
		}
	}
	return newResponse(200, "OK", req)
}

// HandlePlay starts playback (subscribing) on the stream.
func (h *Handler) HandlePlay(req *Request, session *RTSPSession, remoteAddr string) *Response {
	if session != nil && h.server != nil {
		if !session.CanHandleRequest() {
			return newResponse(454, "Session Not Found", req)
		}
		snapshot := session.Snapshot()
		if snapshot.Stream == nil || !snapshot.Startup.Ready ||
			!snapshot.Stream.IsPublisherGeneration(snapshot.Startup.Generation) {
			return newResponse(455, "Method Not Valid in This State", req)
		}
		// Authorization runs before subscriber mutation. The asynchronous start
		// event is emitted only after runSubscriberLoop installs the subscriber.
		if err := h.server.GetEventBus().EmitSync(core.EventSubscribe, &core.EventContext{
			StreamKey:    snapshot.StreamKey,
			SubscriberID: snapshot.ID,
			Protocol:     "rtsp",
			RemoteAddr:   remoteAddr,
		}); err != nil {
			return newResponse(401, "Unauthorized", req)
		}
	}
	if session != nil {
		if err := session.Transition(StatePlaying); err != nil {
			return newResponse(455, "Method Not Valid in This State", req)
		}
	}
	resp := newResponse(200, "OK", req)
	resp.Headers.Set("RTP-Info", "url="+req.URL)
	return resp
}

// HandlePause pauses playback. For live streams, returns 200 OK.
func (h *Handler) HandlePause(req *Request, session *RTSPSession) *Response {
	if session != nil {
		if !session.CanHandleRequest() {
			return newResponse(454, "Session Not Found", req)
		}
		if err := session.Transition(StateReady); err != nil {
			return newResponse(455, "Method Not Valid in This State", req)
		}
	}
	return newResponse(200, "OK", req)
}

// HandleTeardown closes the session.
func (h *Handler) HandleTeardown(req *Request, session *RTSPSession) *Response {
	if session != nil {
		if !session.CanHandleRequest() {
			return newResponse(454, "Session Not Found", req)
		}
		if err := session.Transition(StateClosed); err != nil {
			return newResponse(455, "Method Not Valid in This State", req)
		}
	}
	return newResponse(200, "OK", req)
}

// extractStreamKey extracts the stream key from an RTSP URL.
// e.g., "rtsp://host:554/live/test" -> "live/test"
// e.g., "rtsp://host/live/test/trackID=0" -> "live/test"
func extractStreamKey(rawURL string) string {
	// Remove rtsp:// prefix and host
	idx := strings.Index(rawURL, "://")
	if idx >= 0 {
		rawURL = rawURL[idx+3:]
	}
	// Remove host:port
	slashIdx := strings.Index(rawURL, "/")
	if slashIdx >= 0 {
		rawURL = rawURL[slashIdx+1:]
	}
	// Remove trackID suffix
	if trackIdx := strings.Index(rawURL, "/trackID="); trackIdx >= 0 {
		rawURL = rawURL[:trackIdx]
	}
	return rawURL
}

// parseTransportHeader parses the RTSP Transport header.
func parseTransportHeader(transport string) TransportConfig {
	tc := TransportConfig{}
	parts := strings.Split(transport, ";")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "RTP/AVP/TCP" {
			tc.IsTCP = true
		}
		if part == "multicast" {
			tc.IsMulticast = true
		}
		if strings.HasPrefix(part, "interleaved=") {
			fmt.Sscanf(part, "interleaved=%d-%d", &tc.Interleaved[0], &tc.Interleaved[1])
		}
		if strings.HasPrefix(part, "client_port=") {
			fmt.Sscanf(part, "client_port=%d-%d", &tc.ClientPorts[0], &tc.ClientPorts[1])
		}
	}
	return tc
}
