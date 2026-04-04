package play

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"context"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/sdp"
	pionrtp "github.com/pion/rtp/v2"
)

const (
	rtspPlayDialTimeout = 5 * time.Second
	rtspPlayDefaultPort = "554"
)

// encodingNameToCodec maps SDP encoding names to avframe codec types.
var encodingNameToCodec = map[string]avframe.CodecType{
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

// rtspPlayer implements Player for the RTSP protocol using TCP interleaved
// framing. It performs the DESCRIBE/SETUP/PLAY handshake and then reads
// RTP-depacketized media frames.
type rtspPlayer struct{}

// Play connects to the target RTSP server, subscribes to the stream, and
// delivers received media frames to onFrame. It respects ctx cancellation and
// cfg.Duration limits.
func (p *rtspPlayer) Play(ctx context.Context, cfg PlayConfig, onFrame FrameCallback) error {
	targetURL, addr, err := parseRTSPPlayTarget(cfg)
	if err != nil {
		return fmt.Errorf("parse RTSP URL: %w", err)
	}

	conn, err := dialRTSPPlay(ctx, addr)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}

	// Close connection on context cancel to unblock blocking reads.
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

	rc := &rtspPlayClient{conn: conn, br: bufio.NewReader(conn), cseq: 0}

	// DESCRIBE: get SDP from server.
	resp, err := rc.doRequestWithResponse("DESCRIBE", targetURL, map[string]string{
		"Accept": "application/sdp",
	}, nil)
	if err != nil {
		return fmt.Errorf("DESCRIBE: %w", err)
	}

	sd, err := sdp.Parse(resp.body)
	if err != nil {
		return fmt.Errorf("parse SDP: %w", err)
	}

	ptMap := buildPlayPTMap(sd)

	// SETUP each media track with TCP interleaved transport.
	channel := uint8(0)
	for i := range sd.Media {
		transport := fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", channel, channel+1)
		trackURL := fmt.Sprintf("%s/trackID=%d", targetURL, i)
		if err := rc.doRequest("SETUP", trackURL, map[string]string{
			"Transport": transport,
		}, nil); err != nil {
			return fmt.Errorf("SETUP track %d: %w", i, err)
		}
		channel += 2
	}

	// PLAY: begin media delivery.
	if err := rc.doRequest("PLAY", targetURL, map[string]string{
		"Range": "npt=0.000-",
	}, nil); err != nil {
		return fmt.Errorf("PLAY: %w", err)
	}

	// Build depacketizers per payload type.
	depacketizers := make(map[uint8]pkgrtp.Depacketizer)
	for pt, codec := range ptMap {
		dp, err := pkgrtp.NewDepacketizer(codec)
		if err == nil {
			depacketizers[pt] = dp
		}
	}

	// Determine deadline from cfg.Duration.
	var deadline time.Time
	if cfg.Duration > 0 {
		deadline = time.Now().Add(cfg.Duration)
	}

	// Read loop: parse interleaved frames and depacketize RTP into AVFrames.
	for {
		if ctx.Err() != nil {
			_ = rc.sendTeardown(targetURL)
			return ctx.Err()
		}

		if !deadline.IsZero() && time.Now().After(deadline) {
			_ = rc.sendTeardown(targetURL)
			return nil
		}

		// Set read deadline to avoid blocking forever.
		conn.SetReadDeadline(time.Now().Add(1 * time.Second))

		ch, data, err := rtspReadInterleaved(rc.br)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == io.EOF {
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

		onFrame(frame)
	}
}

// parseRTSPPlayTarget parses an RTSP URL from PlayConfig and returns the
// canonical URL (with optional token), the host:port for dialing, and any error.
func parseRTSPPlayTarget(cfg PlayConfig) (targetURL, addr string, err error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return "", "", fmt.Errorf("parse URL: %w", err)
	}
	if u.Scheme != "rtsp" {
		return "", "", fmt.Errorf("unsupported scheme %q, expected rtsp", u.Scheme)
	}

	addr = u.Host
	if u.Port() == "" {
		addr += ":" + rtspPlayDefaultPort
	}

	// Append token as query parameter if provided.
	targetURL = cfg.URL
	if cfg.Token != "" {
		if strings.Contains(targetURL, "?") {
			targetURL += "&token=" + cfg.Token
		} else {
			targetURL += "?token=" + cfg.Token
		}
	}

	return targetURL, addr, nil
}

// dialRTSPPlay establishes a TCP connection to the RTSP server.
func dialRTSPPlay(ctx context.Context, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: rtspPlayDialTimeout}
	return dialer.DialContext(ctx, "tcp", addr)
}

// buildPlayPTMap creates a payload-type-to-codec map from a parsed SDP.
func buildPlayPTMap(sd *sdp.SessionDescription) map[uint8]avframe.CodecType {
	ptMap := make(map[uint8]avframe.CodecType)
	for _, md := range sd.Media {
		for _, pt := range md.Formats {
			rtpMap := md.RTPMap(pt)
			if rtpMap == nil {
				continue
			}
			codec := encodingNameToCodec[strings.ToUpper(rtpMap.EncodingName)]
			if codec != 0 && pt >= 0 && pt <= 127 {
				ptMap[uint8(pt)] = codec
			}
		}
	}
	return ptMap
}

// --- Minimal RTSP client for play ---

type rtspPlayClient struct {
	conn net.Conn
	br   *bufio.Reader
	cseq int
}

type rtspPlayResponse struct {
	statusCode int
	headers    http.Header
	body       []byte
}

func (rc *rtspPlayClient) doRequest(method, reqURL string, headers map[string]string, body []byte) error {
	_, err := rc.doRequestWithResponse(method, reqURL, headers, body)
	return err
}

func (rc *rtspPlayClient) doRequestWithResponse(method, reqURL string, headers map[string]string, body []byte) (*rtspPlayResponse, error) {
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
		return nil, fmt.Errorf("write request: %w", err)
	}
	if body != nil {
		if _, err := rc.conn.Write(body); err != nil {
			return nil, fmt.Errorf("write body: %w", err)
		}
	}

	return rc.readResponse()
}

func (rc *rtspPlayClient) readResponse() (*rtspPlayResponse, error) {
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

	resp := &rtspPlayResponse{
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

// sendTeardown sends a TEARDOWN request. Errors are ignored since this is
// best-effort cleanup.
func (rc *rtspPlayClient) sendTeardown(reqURL string) error {
	rc.cseq++
	msg := fmt.Sprintf("TEARDOWN %s RTSP/1.0\r\nCSeq: %d\r\n\r\n", reqURL, rc.cseq)
	_, err := rc.conn.Write([]byte(msg))
	return err
}

// rtspReadInterleaved reads a TCP interleaved frame ($ marker). If the first
// byte is not '$', it skips the line (which may be an RTSP response line mixed
// in after PLAY) and retries.
func rtspReadInterleaved(r *bufio.Reader) (channel uint8, data []byte, err error) {
	for {
		marker, err := r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		if marker != '$' {
			// Not an interleaved frame; skip the rest of this line (likely an
			// RTSP response mixed in after PLAY).
			if err := r.UnreadByte(); err != nil {
				return 0, nil, err
			}
			_, err = r.ReadString('\n')
			if err != nil {
				return 0, nil, err
			}
			continue
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
}
