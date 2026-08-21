package rtsp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/portalloc"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

// Handler processes RTSP requests.
type Handler struct {
	server    *core.Server
	ports     *portalloc.PortAllocator
	multicast *config.MulticastConfig // nil if multicast disabled
}

// NewHandler creates a new RTSP handler.
func NewHandler(server *core.Server, ports *portalloc.PortAllocator, multicast *config.MulticastConfig) *Handler {
	return &Handler{server: server, ports: ports, multicast: multicast}
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
	if !ok || stream.Publisher() == nil {
		return newResponse(404, "Stream Not Found", req)
	}
	if err := h.authorize(req, core.AuthorizationSubscribe, streamKey, ""); err != nil {
		return newResponse(401, "Unauthorized", req)
	}
	mediaInfo := stream.Publisher().MediaInfo()
	sd := sdp.BuildFromMediaInfo(mediaInfo, req.URL, "0.0.0.0")
	body := sd.Marshal()
	slog.Debug("DESCRIBE SDP", "module", "rtsp", "body", string(body))
	resp := newResponse(200, "OK", req)
	resp.Headers.Set("Content-Type", "application/sdp")
	resp.Headers.Set("Content-Base", req.URL+"/")
	resp.Headers.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	resp.Body = body
	if session != nil {
		session.setDescription(stream, mediaInfo, sdpToTrackDescriptions(sd))
		session.Transition(StateDescribed)
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
	if session != nil && h.server != nil {
		if err := h.authorize(req, core.AuthorizationSubscribe, session.StreamKey, remoteAddr); err != nil {
			return newResponse(401, "Unauthorized", req)
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
		trackID, _ := extractTrackID(req.URL)
		ts := TrackSetup{
			TrackID:   trackID,
			Transport: tc,
			UDP:       udpTransport,
			Multicast: mcastTransport,
		}
		if desc, ok := trackDescriptionForURL(session.TrackDescriptions, req.URL); ok {
			ts.TrackID = desc.TrackID
			ts.Control = desc.Control
			ts.PayloadType = desc.PayloadType
			ts.Codec = desc.Info.Codec
		} else if session.MediaInfo != nil {
			idx := len(session.trackSnapshot())
			if idx == 0 && session.MediaInfo.HasVideo() {
				ts.Codec = session.MediaInfo.VideoCodec
			} else if (idx == 0 && !session.MediaInfo.HasVideo()) || idx == 1 {
				ts.Codec = session.MediaInfo.AudioCodec
			}
		}
		session.addTrack(ts)
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
		resp.Headers.Set("Session", session.ID+";timeout=60")
		session.Transition(StateReady)
	}
	return resp
}

// HandleAnnounce processes ANNOUNCE request with SDP body.
func (h *Handler) HandleAnnounce(req *Request, session *RTSPSession, remoteAddr string) *Response {
	if len(req.Body) == 0 {
		return newResponse(400, "Bad Request", req)
	}
	sd, err := sdp.Parse(req.Body)
	if err != nil {
		return newResponse(400, "Bad Request", req)
	}

	if session != nil && h.server != nil {
		if err := h.authorize(req, core.AuthorizationPublish, session.StreamKey, remoteAddr); err != nil {
			return newResponse(401, "Unauthorized", req)
		}

		mediaInfo, ptMap := sdpToMediaInfoWithPT(sd)
		descriptions := sdpToTrackDescriptions(sd)
		session.setDescription(nil, mediaInfo, descriptions)

		stream, err := h.server.StreamHub().GetOrCreate(session.StreamKey)
		if err != nil {
			return newResponse(503, "Service Unavailable", req)
		}
		pub, err := NewRTSPPublisherWithDescriptions(session.ID, mediaInfo, stream, descriptions)
		if len(descriptions) == 0 {
			pub, err = NewRTSPPublisherWithTracks(session.ID, mediaInfo, stream, ptMap)
		}
		if err != nil {
			return newResponse(500, "Internal Server Error", req)
		}
		generation, err := stream.SetPublisherWithGeneration(pub)
		if err != nil {
			return newResponse(500, "Internal Server Error", req)
		}
		session.setPublishState(stream, pub, generation)
		h.server.GetEventBus().Emit(core.EventPublish, &core.EventContext{ //nolint:errcheck
			StreamKey: session.StreamKey, Protocol: "rtsp", RemoteAddr: remoteAddr,
			Params: rtspQueryParams(req.URL),
		})

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
			stream.WriteFrame(seqFrame)
			slog.Debug("injected SPS/PPS from SDP", "module", "rtsp", "bytes", len(mediaInfo.VideoSequenceHeader))
		}
	}

	if session != nil {
		session.Transition(StateAnnounced)
	}
	return newResponse(200, "OK", req)
}

// HandleRecord starts recording (publishing) on the stream.
func (h *Handler) HandleRecord(req *Request, session *RTSPSession) *Response {
	if session != nil {
		if err := session.Transition(StateRecording); err != nil {
			return newResponse(455, "Method Not Valid in This State", req)
		}
	}
	return newResponse(200, "OK", req)
}

// HandlePlay starts playback (subscribing) on the stream.
func (h *Handler) HandlePlay(req *Request, session *RTSPSession, remoteAddr string) *Response {
	if session != nil && h.server != nil {
		if err := h.authorize(req, core.AuthorizationSubscribe, session.StreamKey, remoteAddr); err != nil {
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

func (h *Handler) authorize(req *Request, action core.AuthorizationAction, streamKey, remoteAddr string) error {
	request := core.AuthorizationRequest{
		Action: action, StreamKey: streamKey, Protocol: "rtsp",
		RemoteAddr: remoteAddr, Params: rtspQueryParams(req.URL),
	}
	request.Stage = core.AuthorizationPreSession
	if err := h.server.Authorize(context.Background(), request); err != nil {
		return err
	}
	request.Stage = core.AuthorizationPostConnect
	return h.server.Authorize(context.Background(), request)
}

func rtspQueryParams(rawURL string) map[string]string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	params := make(map[string]string)
	for name, values := range parsed.Query() {
		if len(values) > 0 {
			params[name] = values[0]
		}
	}
	if len(params) == 0 {
		return nil
	}
	return params
}

func trackDescriptionForURL(descriptions []RTPTrackDescription, rawURL string) (RTPTrackDescription, bool) {
	if id, ok := extractTrackID(rawURL); ok {
		for _, description := range descriptions {
			if description.TrackID == id {
				return description, true
			}
		}
	}
	control := extractTrackControl(rawURL)
	if control == "" {
		return RTPTrackDescription{}, false
	}
	for _, description := range descriptions {
		if normalizeTrackControl(description.Control) == control {
			return description, true
		}
	}
	return RTPTrackDescription{}, false
}

func extractTrackControl(rawURL string) string {
	if parsed, err := url.Parse(rawURL); err == nil && parsed.Path != "" {
		rawURL = parsed.Path
	}
	rawURL = strings.Trim(rawURL, "/")
	if rawURL == "" {
		return ""
	}
	if idx := strings.LastIndexByte(rawURL, '/'); idx >= 0 {
		rawURL = rawURL[idx+1:]
	}
	return normalizeTrackControl(rawURL)
}

func normalizeTrackControl(raw string) string {
	if parsed, err := url.Parse(raw); err == nil && parsed.Path != "" {
		raw = parsed.Path
	}
	raw = strings.Trim(raw, "/")
	if idx := strings.LastIndexByte(raw, '/'); idx >= 0 {
		raw = raw[idx+1:]
	}
	if idx := strings.IndexByte(raw, '?'); idx >= 0 {
		raw = raw[:idx]
	}
	return raw
}

// HandlePause pauses playback. For live streams, returns 200 OK.
func (h *Handler) HandlePause(req *Request, session *RTSPSession) *Response {
	if session != nil {
		session.Transition(StateReady)
	}
	return newResponse(200, "OK", req)
}

// HandleTeardown closes the session.
func (h *Handler) HandleTeardown(req *Request, session *RTSPSession) *Response {
	if session != nil {
		session.Transition(StateClosed)
	}
	return newResponse(200, "OK", req)
}

// extractStreamKey extracts the stream key from an RTSP URL.
// e.g., "rtsp://host:554/live/test" -> "live/test"
// e.g., "rtsp://host/live/test/trackID=0" -> "live/test"
func extractStreamKey(rawURL string) string {
	if parsed, err := url.Parse(rawURL); err == nil && parsed.Path != "" {
		rawURL = parsed.Path
	}
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
	if queryIdx := strings.IndexByte(rawURL, '?'); queryIdx >= 0 {
		rawURL = rawURL[:queryIdx]
	}
	if fragmentIdx := strings.IndexByte(rawURL, '#'); fragmentIdx >= 0 {
		rawURL = rawURL[:fragmentIdx]
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
