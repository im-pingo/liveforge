package push

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	"github.com/im-pingo/liveforge/pkg/sdp"
	"github.com/im-pingo/liveforge/tools/testkit/report"
	"github.com/im-pingo/liveforge/tools/testkit/source"
)

const (
	rtspDialTimeout = 5 * time.Second
	rtspDefaultPort = "554"
	rtspMTU         = 1400
)

// rtspPusher implements Pusher for the RTSP protocol using TCP interleaved
// framing. It performs the ANNOUNCE/SETUP/RECORD handshake and then sends
// RTP-packetized media over the TCP connection.
type rtspPusher struct{}

// Push connects to the target RTSP server, publishes frames from src via the
// ANNOUNCE/SETUP/RECORD flow, and returns a PushReport with statistics.
func (p *rtspPusher) Push(ctx context.Context, src source.Source, cfg PushConfig) (*report.PushReport, error) {
	targetURL, addr, err := parseRTSPTarget(cfg.Target)
	if err != nil {
		return nil, fmt.Errorf("parse RTSP URL: %w", err)
	}

	conn, err := dialRTSP(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}

	// Ensure the connection is closed when we exit, unblocking any pending I/O.
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

	rc := &rtspPushClient{conn: conn, br: bufio.NewReader(conn), cseq: 0}

	// Build SDP from source media info.
	mi := src.MediaInfo()
	avMI := sourceMediaInfoToAV(mi)
	sd := sdp.BuildFromMediaInfo(avMI, targetURL, "0.0.0.0")
	sdpBody := sd.Marshal()

	// ANNOUNCE: declare the stream with SDP.
	if err := rc.doRequest("ANNOUNCE", targetURL, map[string]string{
		"Content-Type":   "application/sdp",
		"Content-Length": strconv.Itoa(len(sdpBody)),
	}, sdpBody); err != nil {
		return nil, fmt.Errorf("ANNOUNCE: %w", err)
	}

	// SETUP each media track with TCP interleaved transport.
	channel := uint8(0)
	videoChannel, audioChannel := -1, -1
	for i, md := range sd.Media {
		transport := fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", channel, channel+1)
		trackURL := fmt.Sprintf("%s/trackID=%d", targetURL, i)
		if err := rc.doRequest("SETUP", trackURL, map[string]string{
			"Transport": transport,
		}, nil); err != nil {
			return nil, fmt.Errorf("SETUP track %d: %w", i, err)
		}
		if md.Type == "video" {
			videoChannel = int(channel)
		} else if md.Type == "audio" {
			audioChannel = int(channel)
		}
		channel += 2
	}

	// RECORD: begin media transmission.
	if err := rc.doRequest("RECORD", targetURL, map[string]string{
		"Range": "npt=0.000-",
	}, nil); err != nil {
		return nil, fmt.Errorf("RECORD: %w", err)
	}

	// Build RTP packetizers and sessions.
	var videoSession, audioSession *pkgrtp.Session
	var videoPkt, audioPkt pkgrtp.Packetizer

	if mi.HasVideo() {
		videoPkt, _ = pkgrtp.NewPacketizer(mi.VideoCodec)
		videoSession = pkgrtp.NewSession(96, 90000)
	}
	if mi.HasAudio() {
		audioPkt, _ = pkgrtp.NewPacketizer(mi.AudioCodec)
		clockRate := uint32(48000) // default
		if mi.AudioCodec == avframe.CodecAAC {
			clockRate = 44100
		}
		audioSession = pkgrtp.NewSession(97, clockRate)
	}

	// Push frames.
	start := time.Now()
	var framesSent int64
	var bytesSent int64

	var deadline time.Time
	if cfg.Duration > 0 {
		deadline = start.Add(cfg.Duration)
	}

	for {
		select {
		case <-ctx.Done():
			_ = rc.sendTeardown(targetURL)
			return buildPushReport(cfg, start, framesSent, bytesSent), ctx.Err()
		default:
		}

		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}

		frame, err := src.NextFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = rc.sendTeardown(targetURL)
			return buildPushReport(cfg, start, framesSent, bytesSent),
				fmt.Errorf("read source frame: %w", err)
		}

		// Skip sequence headers; the SDP already declares codec parameters.
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

		rtpPackets, err := pkt.Packetize(frame, rtspMTU)
		if err != nil {
			continue
		}
		session.WrapPackets(rtpPackets, frame.DTS)

		for _, rp := range rtpPackets {
			raw, err := rp.Marshal()
			if err != nil {
				continue
			}
			if err := rtspWriteInterleaved(conn, uint8(ch), raw); err != nil {
				if ctx.Err() != nil {
					return buildPushReport(cfg, start, framesSent, bytesSent), ctx.Err()
				}
				_ = rc.sendTeardown(targetURL)
				return buildPushReport(cfg, start, framesSent, bytesSent),
					fmt.Errorf("write interleaved: %w", err)
			}
			bytesSent += int64(len(raw)) + 4 // payload + 4-byte interleaved header
		}

		framesSent++
	}

	_ = rc.sendTeardown(targetURL)
	return buildPushReport(cfg, start, framesSent, bytesSent), nil
}

// parseRTSPTarget parses an RTSP URL and returns the canonical URL string, the
// host:port address for dialing, and any error.
func parseRTSPTarget(rawURL string) (targetURL, addr string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("parse URL: %w", err)
	}
	if u.Scheme != "rtsp" {
		return "", "", fmt.Errorf("unsupported scheme %q, expected rtsp", u.Scheme)
	}
	addr = u.Host
	if u.Port() == "" {
		addr += ":" + rtspDefaultPort
	}
	return rawURL, addr, nil
}

// dialRTSP establishes a TCP connection to the RTSP server.
func dialRTSP(ctx context.Context, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: rtspDialTimeout}
	return dialer.DialContext(ctx, "tcp", addr)
}

// sourceMediaInfoToAV converts a source.MediaInfo to avframe.MediaInfo for SDP
// building. The testkit source.MediaInfo has codec types but not sample rates,
// so we fill in standard defaults.
func sourceMediaInfoToAV(mi *source.MediaInfo) *avframe.MediaInfo {
	info := &avframe.MediaInfo{
		VideoCodec: mi.VideoCodec,
		AudioCodec: mi.AudioCodec,
	}
	// Set default sample rate for audio codecs used in SDP.
	switch mi.AudioCodec {
	case avframe.CodecAAC:
		info.SampleRate = 44100
		info.Channels = 2
	case avframe.CodecOpus:
		info.SampleRate = 48000
		info.Channels = 2
	case avframe.CodecG711U, avframe.CodecG711A:
		info.SampleRate = 8000
		info.Channels = 1
	case avframe.CodecMP3:
		info.SampleRate = 44100
		info.Channels = 2
	}
	return info
}

// --- Minimal RTSP client for push ---

type rtspPushClient struct {
	conn net.Conn
	br   *bufio.Reader
	cseq int
}

func (rc *rtspPushClient) doRequest(method, reqURL string, headers map[string]string, body []byte) error {
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
		return err
	}
	if body != nil {
		if _, err := rc.conn.Write(body); err != nil {
			return err
		}
	}

	return rc.readResponse()
}

func (rc *rtspPushClient) readResponse() error {
	line, err := rc.br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read status line: %w", err)
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return fmt.Errorf("malformed status line: %q", line)
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return fmt.Errorf("parse status code: %w", err)
	}

	hdrs := make(http.Header)
	for {
		hline, err := rc.br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read header: %w", err)
		}
		hline = strings.TrimRight(hline, "\r\n")
		if hline == "" {
			break
		}
		idx := strings.Index(hline, ":")
		if idx < 0 {
			continue
		}
		hdrs.Add(strings.TrimSpace(hline[:idx]), strings.TrimSpace(hline[idx+1:]))
	}

	// Consume response body if present.
	if cl := hdrs.Get("Content-Length"); cl != "" {
		length, err := strconv.Atoi(cl)
		if err == nil && length > 0 {
			body := make([]byte, length)
			if _, err := io.ReadFull(rc.br, body); err != nil {
				return fmt.Errorf("read body: %w", err)
			}
		}
	}

	if code >= 400 {
		return fmt.Errorf("RTSP error %d", code)
	}

	return nil
}

// sendTeardown sends a TEARDOWN request. Errors are ignored since this is
// best-effort cleanup.
func (rc *rtspPushClient) sendTeardown(reqURL string) error {
	rc.cseq++
	msg := fmt.Sprintf("TEARDOWN %s RTSP/1.0\r\nCSeq: %d\r\n\r\n", reqURL, rc.cseq)
	_, err := rc.conn.Write([]byte(msg))
	return err
}

// rtspWriteInterleaved writes an RTP packet using TCP interleaved framing:
// '$' + 1-byte channel + 2-byte big-endian length + payload.
func rtspWriteInterleaved(w io.Writer, channel uint8, data []byte) error {
	header := [4]byte{'$', channel, 0, 0}
	binary.BigEndian.PutUint16(header[2:], uint16(len(data)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}
