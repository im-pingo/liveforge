package cluster

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/sdp"
	pionrtp "github.com/pion/rtp/v2"
)

// RTSPTransport implements RelayTransport for RTSP protocol.
// It uses TCP interleaved framing for RTP data transport.
type RTSPTransport struct {
	cfg config.ClusterRTSPConfig
}

// NewRTSPTransport creates a new RTSP relay transport.
func NewRTSPTransport(cfg config.ClusterRTSPConfig) *RTSPTransport {
	return &RTSPTransport{cfg: cfg}
}

func (t *RTSPTransport) Scheme() string { return "rtsp" }

func (t *RTSPTransport) Push(ctx context.Context, targetURL string, stream *core.Stream) error {
	u, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	addr := u.Host
	if u.Port() == "" {
		addr += ":554"
	}

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}

	// Close conn on context cancel to unblock any blocking write.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
			conn.Close()
		}
	}()

	rc := &rtspClient{conn: conn, br: bufio.NewReader(conn), cseq: 0}

	pub := stream.Publisher()
	if pub == nil {
		return fmt.Errorf("stream has no publisher")
	}
	mi := pub.MediaInfo()
	sd := sdp.BuildFromMediaInfo(mi, targetURL, "0.0.0.0")
	sdpBody := sd.Marshal()

	// ANNOUNCE
	if err := rc.doRequest("ANNOUNCE", targetURL, map[string]string{
		"Content-Type":   "application/sdp",
		"Content-Length": strconv.Itoa(len(sdpBody)),
	}, sdpBody); err != nil {
		return fmt.Errorf("ANNOUNCE: %w", err)
	}

	// SETUP for each track (TCP interleaved)
	channel := uint8(0)
	videoChannel, audioChannel := -1, -1
	for i, md := range sd.Media {
		transport := fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", channel, channel+1)
		trackURL := fmt.Sprintf("%s/trackID=%d", targetURL, i)
		if err := rc.doRequest("SETUP", trackURL, map[string]string{
			"Transport": transport,
		}, nil); err != nil {
			return fmt.Errorf("SETUP track %d: %w", i, err)
		}
		if md.Type == "video" {
			videoChannel = int(channel)
		} else if md.Type == "audio" {
			audioChannel = int(channel)
		}
		channel += 2
	}

	// RECORD
	if err := rc.doRequest("RECORD", targetURL, map[string]string{
		"Range": "npt=0.000-",
	}, nil); err != nil {
		return fmt.Errorf("RECORD: %w", err)
	}

	slog.Info("rtsp relay push connected", "module", "cluster", "target", targetURL)

	// Build packetizers and sessions
	var videoSession, audioSession *pkgrtp.Session
	var videoPkt, audioPkt pkgrtp.Packetizer

	if mi.VideoCodec != 0 {
		videoPkt, _ = pkgrtp.NewPacketizer(mi.VideoCodec)
		videoSession = pkgrtp.NewSession(96, 90000) // video typically PT 96, 90kHz
	}
	if mi.AudioCodec != 0 {
		audioPkt, _ = pkgrtp.NewPacketizer(mi.AudioCodec)
		clockRate := uint32(mi.SampleRate)
		if clockRate == 0 {
			clockRate = 48000
		}
		audioSession = pkgrtp.NewSession(97, clockRate)
	}

	reader := stream.RingBuffer().NewReader()
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		frame, ok := reader.TryRead()
		if !ok {
			if stream.RingBuffer().IsClosed() {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-stream.RingBuffer().Signal():
			}
			continue
		}

		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			continue
		}

		var pkt pkgrtp.Packetizer
		var session *pkgrtp.Session
		var ch int

		if frame.MediaType.IsVideo() && videoPkt != nil && videoChannel >= 0 {
			pkt = videoPkt
			session = videoSession
			ch = videoChannel
		} else if frame.MediaType.IsAudio() && audioPkt != nil && audioChannel >= 0 {
			pkt = audioPkt
			session = audioSession
			ch = audioChannel
		} else {
			continue
		}

		rtpPackets, err := pkt.Packetize(frame, pkgrtp.DefaultMTU)
		if err != nil {
			continue
		}
		session.WrapPackets(rtpPackets, frame.DTS)

		for _, p := range rtpPackets {
			raw, err := p.Marshal()
			if err != nil {
				continue
			}
			if err := writeInterleaved(conn, uint8(ch), raw); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("write interleaved: %w", err)
			}
		}
	}
}

func (t *RTSPTransport) Pull(ctx context.Context, sourceURL string, stream *core.Stream) error {
	u, err := url.Parse(sourceURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	addr := u.Host
	if u.Port() == "" {
		addr += ":554"
	}

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}

	// Close conn on context cancel to unblock blocking reads.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
			conn.Close()
		}
	}()

	rc := &rtspClient{conn: conn, br: bufio.NewReader(conn), cseq: 0}

	// DESCRIBE
	resp, err := rc.doRequestWithResponse("DESCRIBE", sourceURL, map[string]string{
		"Accept": "application/sdp",
	}, nil)
	if err != nil {
		return fmt.Errorf("DESCRIBE: %w", err)
	}

	sd, err := sdp.Parse(resp.body)
	if err != nil {
		return fmt.Errorf("parse SDP: %w", err)
	}

	mi := sdpToMediaInfo(sd)
	ptMap := buildPTMap(sd)

	// SETUP for each track (TCP interleaved)
	channel := uint8(0)
	for i := range sd.Media {
		transport := fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", channel, channel+1)
		trackURL := fmt.Sprintf("%s/trackID=%d", sourceURL, i)
		if err := rc.doRequest("SETUP", trackURL, map[string]string{
			"Transport": transport,
		}, nil); err != nil {
			return fmt.Errorf("SETUP track %d: %w", i, err)
		}
		channel += 2
	}

	// PLAY
	if err := rc.doRequest("PLAY", sourceURL, map[string]string{
		"Range": "npt=0.000-",
	}, nil); err != nil {
		return fmt.Errorf("PLAY: %w", err)
	}

	slog.Info("rtsp relay pull connected", "module", "cluster", "source", sourceURL)

	pub := &originPublisher{
		id:   fmt.Sprintf("rtsp-pull-%s", stream.Key()),
		info: mi,
	}
	if err := stream.SetPublisher(pub); err != nil {
		return fmt.Errorf("set publisher: %w", err)
	}
	defer stream.RemovePublisher()

	// Build depacketizers
	depacketizers := make(map[uint8]pkgrtp.Depacketizer)
	for pt, codec := range ptMap {
		dp, err := pkgrtp.NewDepacketizer(codec)
		if err == nil {
			depacketizers[pt] = dp
		}
	}

	for {
		ch, data, err := readInterleaved(rc.br)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read interleaved: %w", err)
		}

		// Only process RTP channels (even numbered); skip RTCP.
		if ch%2 != 0 {
			continue
		}

		var pkt pionrtp.Packet
		if err := pkt.Unmarshal(data); err != nil {
			continue
		}

		dp, ok := depacketizers[pkt.PayloadType]
		if !ok {
			continue
		}

		frame, err := dp.Depacketize(&pkt)
		if err != nil || frame == nil {
			continue
		}

		stream.WriteFrame(frame)
	}
}

func (t *RTSPTransport) Close() error { return nil }

// --- Minimal RTSP client ---

type rtspClient struct {
	conn net.Conn
	br   *bufio.Reader
	cseq int
}

type rtspResponse struct {
	statusCode int
	headers    http.Header
	body       []byte
}

func (rc *rtspClient) doRequest(method, reqURL string, headers map[string]string, body []byte) error {
	_, err := rc.doRequestWithResponse(method, reqURL, headers, body)
	return err
}

func (rc *rtspClient) doRequestWithResponse(method, reqURL string, headers map[string]string, body []byte) (*rtspResponse, error) {
	rc.cseq++

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s %s RTSP/1.0\r\n", method, reqURL))
	sb.WriteString(fmt.Sprintf("CSeq: %d\r\n", rc.cseq))
	for k, v := range headers {
		sb.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}
	if body != nil && headers["Content-Length"] == "" {
		sb.WriteString(fmt.Sprintf("Content-Length: %d\r\n", len(body)))
	}
	sb.WriteString("\r\n")

	if _, err := rc.conn.Write([]byte(sb.String())); err != nil {
		return nil, err
	}
	if body != nil {
		if _, err := rc.conn.Write(body); err != nil {
			return nil, err
		}
	}

	return rc.readResponse()
}

func (rc *rtspClient) readResponse() (*rtspResponse, error) {
	line, err := rc.br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read status line: %w", err)
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed status line: %q", line)
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("parse status code: %w", err)
	}

	resp := &rtspResponse{
		statusCode: code,
		headers:    make(http.Header),
	}

	for {
		hline, err := rc.br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read header: %w", err)
		}
		hline = strings.TrimRight(hline, "\r\n")
		if hline == "" {
			break
		}
		idx := strings.Index(hline, ":")
		if idx < 0 {
			continue
		}
		resp.headers.Add(strings.TrimSpace(hline[:idx]), strings.TrimSpace(hline[idx+1:]))
	}

	if cl := resp.headers.Get("Content-Length"); cl != "" {
		length, err := strconv.Atoi(cl)
		if err == nil && length > 0 {
			resp.body = make([]byte, length)
			if _, err := io.ReadFull(rc.br, resp.body); err != nil {
				return nil, fmt.Errorf("read body: %w", err)
			}
		}
	}

	if resp.statusCode >= 400 {
		return resp, fmt.Errorf("RTSP error %d", resp.statusCode)
	}

	return resp, nil
}

// writeInterleaved writes an RTP/RTCP packet using TCP interleaved framing ($).
func writeInterleaved(w io.Writer, channel uint8, data []byte) error {
	header := [4]byte{'$', channel, 0, 0}
	binary.BigEndian.PutUint16(header[2:], uint16(len(data)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// readInterleaved reads a TCP interleaved frame ($ marker).
func readInterleaved(r *bufio.Reader) (channel uint8, data []byte, err error) {
	marker, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	if marker != '$' {
		return 0, nil, fmt.Errorf("expected '$' marker, got 0x%02x", marker)
	}
	channel, err = r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint16(lenBuf[:])
	data = make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return channel, data, nil
}

// sdpToMediaInfo extracts MediaInfo from a parsed SDP.
func sdpToMediaInfo(sd *sdp.SessionDescription) *avframe.MediaInfo {
	info := &avframe.MediaInfo{}
	for _, md := range sd.Media {
		if len(md.Formats) == 0 {
			continue
		}
		pt := md.Formats[0]
		rtpMap := md.RTPMap(pt)
		if rtpMap == nil {
			continue
		}
		codec := encodingNameToCodec(rtpMap.EncodingName)
		if codec == 0 {
			continue
		}
		switch md.Type {
		case "video":
			if info.VideoCodec == 0 {
				info.VideoCodec = codec
			}
		case "audio":
			if info.AudioCodec == 0 {
				info.AudioCodec = codec
				info.SampleRate = rtpMap.ClockRate
			}
		}
	}
	return info
}

// buildPTMap creates a payload-type-to-codec map from SDP.
func buildPTMap(sd *sdp.SessionDescription) map[uint8]avframe.CodecType {
	ptMap := make(map[uint8]avframe.CodecType)
	for _, md := range sd.Media {
		for _, pt := range md.Formats {
			rtpMap := md.RTPMap(pt)
			if rtpMap == nil {
				continue
			}
			codec := encodingNameToCodec(rtpMap.EncodingName)
			if codec != 0 && pt >= 0 && pt <= 127 {
				ptMap[uint8(pt)] = codec
			}
		}
	}
	return ptMap
}

var encodingNames = map[string]avframe.CodecType{
	"H264":          avframe.CodecH264,
	"H265":          avframe.CodecH265,
	"VP8":           avframe.CodecVP8,
	"VP9":           avframe.CodecVP9,
	"AV1":           avframe.CodecAV1,
	"MPEG4-GENERIC": avframe.CodecAAC,
	"MP4A-LATM":     avframe.CodecAAC,
	"OPUS":          avframe.CodecOpus,
	"MPA":           avframe.CodecMP3,
	"PCMU":          avframe.CodecG711U,
	"PCMA":          avframe.CodecG711A,
}

func encodingNameToCodec(name string) avframe.CodecType {
	return encodingNames[strings.ToUpper(name)]
}

func defaultClusterRTSPConfig() config.ClusterRTSPConfig {
	return config.ClusterRTSPConfig{Transport: "tcp"}
}
