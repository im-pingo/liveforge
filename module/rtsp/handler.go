package rtsp

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

// Handler processes RTSP requests.
type Handler struct {
	server      *core.Server
	portManager *PortManager
}

// NewHandler creates a new RTSP handler.
func NewHandler(server *core.Server, pm *PortManager) *Handler {
	return &Handler{server: server, portManager: pm}
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
	mediaInfo := stream.Publisher().MediaInfo()
	sd := sdp.BuildFromMediaInfo(mediaInfo, req.URL, "0.0.0.0")
	body := sd.Marshal()
	log.Printf("rtsp: DESCRIBE SDP:\n%s", string(body))
	resp := newResponse(200, "OK", req)
	resp.Headers.Set("Content-Type", "application/sdp")
	resp.Headers.Set("Content-Base", req.URL+"/")
	resp.Headers.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	resp.Body = body
	if session != nil {
		session.MediaInfo = mediaInfo
		session.Stream = stream
		session.Transition(StateDescribed)
	}
	return resp
}

// TransportConfig holds parsed Transport header data.
type TransportConfig struct {
	IsTCP       bool
	Interleaved [2]int // channel pair for TCP
	ClientPorts [2]int // client ports for UDP
	ServerPorts [2]int // allocated server ports for UDP
}

// HandleSetup negotiates transport for a track.
func (h *Handler) HandleSetup(req *Request, session *RTSPSession) *Response {
	transport := req.Headers.Get("Transport")
	tc := parseTransportHeader(transport)

	if !tc.IsTCP && h.portManager != nil {
		rtp, rtcp, err := h.portManager.Allocate()
		if err != nil {
			return newResponse(500, "Internal Server Error", req)
		}
		tc.ServerPorts = [2]int{rtp, rtcp}
	}

	// Store transport config per track on the session.
	if session != nil {
		trackID, _ := extractTrackID(req.URL)
		ts := TrackSetup{
			TrackID:   trackID,
			Transport: tc,
		}
		// Assign codec from MediaInfo based on track order.
		if session.MediaInfo != nil {
			idx := len(session.Tracks)
			if idx == 0 && session.MediaInfo.HasVideo() {
				ts.Codec = session.MediaInfo.VideoCodec
			} else if (idx == 0 && !session.MediaInfo.HasVideo()) || idx == 1 {
				ts.Codec = session.MediaInfo.AudioCodec
			}
		}
		session.Tracks = append(session.Tracks, ts)
	}

	resp := newResponse(200, "OK", req)
	if tc.IsTCP {
		resp.Headers.Set("Transport", fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", tc.Interleaved[0], tc.Interleaved[1]))
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
func (h *Handler) HandleAnnounce(req *Request, session *RTSPSession) *Response {
	if len(req.Body) == 0 {
		return newResponse(400, "Bad Request", req)
	}
	sd, err := sdp.Parse(req.Body)
	if err != nil {
		return newResponse(400, "Bad Request", req)
	}

	if session != nil && h.server != nil {
		mediaInfo := sdpToMediaInfo(sd)
		session.MediaInfo = mediaInfo

		stream := h.server.StreamHub().GetOrCreate(session.StreamKey)
		session.Stream = stream

		pub, err := NewRTSPPublisher(session.ID, mediaInfo, stream)
		if err != nil {
			return newResponse(500, "Internal Server Error", req)
		}
		session.Publisher = pub

		if err := stream.SetPublisher(pub); err != nil {
			return newResponse(500, "Internal Server Error", req)
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
			stream.WriteFrame(seqFrame)
			log.Printf("rtsp: injected SPS/PPS sequence header from SDP (%d bytes)", len(mediaInfo.VideoSequenceHeader))
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
func (h *Handler) HandlePlay(req *Request, session *RTSPSession) *Response {
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
		if strings.HasPrefix(part, "interleaved=") {
			fmt.Sscanf(part, "interleaved=%d-%d", &tc.Interleaved[0], &tc.Interleaved[1])
		}
		if strings.HasPrefix(part, "client_port=") {
			fmt.Sscanf(part, "client_port=%d-%d", &tc.ClientPorts[0], &tc.ClientPorts[1])
		}
	}
	return tc
}
