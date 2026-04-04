package push

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/im-pingo/liveforge/module/rtmp"
	"github.com/im-pingo/liveforge/pkg/avframe"
	flvpkg "github.com/im-pingo/liveforge/pkg/muxer/flv"
	"github.com/im-pingo/liveforge/tools/testkit/report"
	"github.com/im-pingo/liveforge/tools/testkit/source"
)

const (
	handshakeSize    = 1536
	defaultChunkSize = 4096
	connectTimeout   = 5 * time.Second
)

// rtmpPusher implements Pusher for the RTMP protocol.
type rtmpPusher struct{}

// rtmpConn wraps a net.Conn with RTMP chunk reader/writer.
type rtmpConn struct {
	conn net.Conn
	cr   *rtmp.ChunkReader
	cw   *rtmp.ChunkWriter
}

// Push connects to the target RTMP server, publishes frames from src, and
// returns a PushReport with statistics. It respects ctx cancellation and
// cfg.Duration limits.
func (p *rtmpPusher) Push(ctx context.Context, src source.Source, cfg PushConfig) (*report.PushReport, error) {
	host, app, streamName, err := parseRTMPURL(cfg.Target)
	if err != nil {
		return nil, fmt.Errorf("parse RTMP URL: %w", err)
	}

	// Append token to stream name if provided.
	if cfg.Token != "" {
		streamName += "?token=" + cfg.Token
	}

	rc, err := dialRTMP(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", host, err)
	}
	defer rc.conn.Close()

	// RTMP command sequence: connect -> read response -> set chunk size ->
	// publish commands -> read responses -> send media.
	if err := rc.sendConnect(app, host); err != nil {
		return nil, fmt.Errorf("send connect: %w", err)
	}

	// Read connect response (transaction ID 1).
	if err := rc.readResponses(1); err != nil {
		return nil, fmt.Errorf("connect response: %w", err)
	}

	// Set chunk size for better throughput.
	if err := rc.setChunkSize(defaultChunkSize); err != nil {
		return nil, fmt.Errorf("set chunk size: %w", err)
	}

	// Send publish command sequence (releaseStream, FCPublish, createStream).
	if err := rc.sendPublish(streamName); err != nil {
		return nil, fmt.Errorf("send publish: %w", err)
	}

	// Read createStream response (transaction ID 4).
	if err := rc.readResponses(4); err != nil {
		return nil, fmt.Errorf("createStream response: %w", err)
	}

	// Send the actual publish command on stream ID 1.
	if err := rc.sendPublishCommand(streamName); err != nil {
		return nil, fmt.Errorf("send publish command: %w", err)
	}

	// Read publish response (onStatus).
	if err := rc.readResponses(0); err != nil {
		return nil, fmt.Errorf("publish response: %w", err)
	}

	// Push frames.
	start := time.Now()
	var framesSent int64
	var bytesSent int64

	// Determine deadline from cfg.Duration.
	var deadline time.Time
	if cfg.Duration > 0 {
		deadline = start.Add(cfg.Duration)
	}

	for {
		// Check context cancellation.
		select {
		case <-ctx.Done():
			return buildPushReport(cfg, start, framesSent, bytesSent), ctx.Err()
		default:
		}

		// Check duration limit.
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}

		frame, err := src.NextFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			return buildPushReport(cfg, start, framesSent, bytesSent),
				fmt.Errorf("read source frame: %w", err)
		}

		n, err := rc.sendMediaFrame(frame)
		if err != nil {
			return buildPushReport(cfg, start, framesSent, bytesSent),
				fmt.Errorf("send media frame: %w", err)
		}

		framesSent++
		bytesSent += int64(n)
	}

	return buildPushReport(cfg, start, framesSent, bytesSent), nil
}

// dialRTMP connects to an RTMP server, performs the handshake, and returns an
// initialized rtmpConn. It respects context cancellation for the dial phase.
func dialRTMP(ctx context.Context, addr string) (*rtmpConn, error) {
	dialer := &net.Dialer{Timeout: connectTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	if err := clientHandshake(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}

	return &rtmpConn{
		conn: conn,
		cr:   rtmp.NewChunkReader(conn, rtmp.DefaultChunkSize),
		cw:   rtmp.NewChunkWriter(conn, defaultChunkSize),
	}, nil
}

// clientHandshake performs the RTMP client-side handshake (simple mode).
func clientHandshake(conn net.Conn) error {
	// Send C0 (version 3) + C1 (1536 random bytes).
	c0c1 := make([]byte, 1+handshakeSize)
	c0c1[0] = 3
	rand.Read(c0c1[1:])

	if _, err := conn.Write(c0c1); err != nil {
		return fmt.Errorf("write C0C1: %w", err)
	}

	// Read S0 + S1 + S2.
	s0s1s2 := make([]byte, 1+handshakeSize+handshakeSize)
	if _, err := io.ReadFull(conn, s0s1s2); err != nil {
		return fmt.Errorf("read S0S1S2: %w", err)
	}

	// Send C2 (echo S1).
	c2 := s0s1s2[1 : 1+handshakeSize]
	if _, err := conn.Write(c2); err != nil {
		return fmt.Errorf("write C2: %w", err)
	}

	return nil
}

// sendConnect sends the RTMP connect command.
func (rc *rtmpConn) sendConnect(app, host string) error {
	payload, err := rtmp.AMF0Encode(
		"connect",
		float64(1),
		map[string]any{
			"app":      app,
			"type":     "nonprivate",
			"flashVer": "FMLE/3.0",
			"tcUrl":    "rtmp://" + host + "/" + app,
		},
	)
	if err != nil {
		return fmt.Errorf("encode connect: %w", err)
	}

	return rc.cw.WriteMessage(3, &rtmp.Message{
		TypeID:  rtmp.MsgAMF0Command,
		Length:  uint32(len(payload)),
		Payload: payload,
	})
}

// sendPublish sends the RTMP publish pre-commands: releaseStream, FCPublish,
// and createStream.
func (rc *rtmpConn) sendPublish(streamName string) error {
	// releaseStream (txn 2).
	payload, _ := rtmp.AMF0Encode("releaseStream", float64(2), nil, streamName)
	if err := rc.cw.WriteMessage(3, &rtmp.Message{
		TypeID: rtmp.MsgAMF0Command, Length: uint32(len(payload)), Payload: payload,
	}); err != nil {
		return fmt.Errorf("releaseStream: %w", err)
	}

	// FCPublish (txn 3).
	payload, _ = rtmp.AMF0Encode("FCPublish", float64(3), nil, streamName)
	if err := rc.cw.WriteMessage(3, &rtmp.Message{
		TypeID: rtmp.MsgAMF0Command, Length: uint32(len(payload)), Payload: payload,
	}); err != nil {
		return fmt.Errorf("FCPublish: %w", err)
	}

	// createStream (txn 4).
	payload, _ = rtmp.AMF0Encode("createStream", float64(4), nil)
	if err := rc.cw.WriteMessage(3, &rtmp.Message{
		TypeID: rtmp.MsgAMF0Command, Length: uint32(len(payload)), Payload: payload,
	}); err != nil {
		return fmt.Errorf("createStream: %w", err)
	}

	return nil
}

// sendPublishCommand sends the actual RTMP publish command on stream ID 1.
func (rc *rtmpConn) sendPublishCommand(streamName string) error {
	payload, _ := rtmp.AMF0Encode("publish", float64(5), nil, streamName, "live")
	return rc.cw.WriteMessage(8, &rtmp.Message{
		TypeID:   rtmp.MsgAMF0Command,
		Length:   uint32(len(payload)),
		StreamID: 1,
		Payload:  payload,
	})
}

// readResponses reads and processes RTMP response messages until a matching
// result is found or an error occurs. When targetTxnID is 0, it looks for an
// onStatus response instead.
func (rc *rtmpConn) readResponses(targetTxnID float64) error {
	for i := 0; i < 20; i++ {
		msg, err := rc.cr.ReadMessage()
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}

		// Handle protocol control messages.
		switch msg.TypeID {
		case rtmp.MsgSetChunkSize:
			if len(msg.Payload) >= 4 {
				size := int(binary.BigEndian.Uint32(msg.Payload))
				rc.cr.SetChunkSize(size)
			}
			continue
		case rtmp.MsgWindowAckSize, rtmp.MsgSetPeerBandwidth, rtmp.MsgAck, rtmp.MsgUserControl:
			continue
		}

		if msg.TypeID != rtmp.MsgAMF0Command {
			continue
		}

		vals, err := rtmp.AMF0Decode(msg.Payload)
		if err != nil || len(vals) < 2 {
			continue
		}

		cmd, _ := vals[0].(string)
		txnID, _ := vals[1].(float64)

		if cmd == "_error" {
			desc := "unknown error"
			if len(vals) >= 4 {
				if m, ok := vals[3].(map[string]any); ok {
					if d, ok := m["description"].(string); ok {
						desc = d
					}
				}
			}
			return fmt.Errorf("RTMP error: %s", desc)
		}

		if cmd == "_result" && txnID == targetTxnID {
			return nil
		}

		if cmd == "onStatus" {
			if len(vals) >= 4 {
				if m, ok := vals[3].(map[string]any); ok {
					code, _ := m["code"].(string)
					if strings.Contains(code, "Failed") || strings.Contains(code, "Reject") {
						return fmt.Errorf("RTMP status: %s", code)
					}
				}
			}
			return nil
		}
	}

	return fmt.Errorf("no response for transaction %v after 20 messages", targetTxnID)
}

// setChunkSize sends a set-chunk-size protocol control message.
func (rc *rtmpConn) setChunkSize(size int) error {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(size))
	return rc.cw.WriteMessage(2, &rtmp.Message{
		TypeID:  rtmp.MsgSetChunkSize,
		Length:  4,
		Payload: payload,
	})
}

// sendMediaFrame sends an AVFrame as an RTMP media message.
// Returns the number of payload bytes written.
func (rc *rtmpConn) sendMediaFrame(frame *avframe.AVFrame) (int, error) {
	payload, err := buildRTMPPayload(frame)
	if err != nil {
		return 0, err
	}
	if payload == nil {
		return 0, nil
	}

	var msgTypeID uint8
	if frame.MediaType.IsVideo() {
		msgTypeID = rtmp.MsgVideo
	} else {
		msgTypeID = rtmp.MsgAudio
	}

	err = rc.cw.WriteMessage(6, &rtmp.Message{
		TypeID:    msgTypeID,
		Length:    uint32(len(payload)),
		Timestamp: uint32(frame.DTS),
		StreamID:  1,
		Payload:   payload,
	})
	if err != nil {
		return 0, err
	}

	return len(payload), nil
}

// buildRTMPPayload converts an AVFrame to RTMP FLV tag body (without the tag
// header). The FLV muxer writes a complete tag; we strip the 11-byte tag header
// and 4-byte trailing previous-tag-size to get just the media data.
func buildRTMPPayload(frame *avframe.AVFrame) ([]byte, error) {
	var buf bytes.Buffer
	muxer := flvpkg.NewMuxer()
	if err := muxer.WriteFrame(&buf, frame); err != nil {
		return nil, err
	}

	tagData := buf.Bytes()
	if len(tagData) < flvpkg.TagHeaderSize+4 {
		return nil, nil
	}

	dataSize := len(tagData) - flvpkg.TagHeaderSize - 4
	if dataSize <= 0 {
		return nil, nil
	}
	result := make([]byte, dataSize)
	copy(result, tagData[flvpkg.TagHeaderSize:flvpkg.TagHeaderSize+dataSize])
	return result, nil
}

// parseRTMPURL parses an RTMP URL into host:port, app, and stream name.
// Format: rtmp://host[:port]/app/streamName[?token=xxx]
func parseRTMPURL(rawURL string) (host, app, streamName string, err error) {
	u := rawURL
	if strings.HasPrefix(u, "rtmp://") {
		u = u[len("rtmp://"):]
	} else {
		return "", "", "", fmt.Errorf("unsupported scheme in %q", rawURL)
	}

	// Strip query string from path parsing; it will be part of streamName
	// if present, but we handle token separately via PushConfig.Token.
	queryIdx := strings.Index(u, "?")
	queryPart := ""
	if queryIdx >= 0 {
		queryPart = u[queryIdx:]
		u = u[:queryIdx]
	}

	// Split host from path.
	slashIdx := strings.Index(u, "/")
	if slashIdx < 0 {
		return "", "", "", fmt.Errorf("no path in URL %q", rawURL)
	}
	host = u[:slashIdx]
	path := u[slashIdx+1:]

	// Default port.
	if !strings.Contains(host, ":") {
		host += ":1935"
	}

	// Split app and stream name: first segment is app, rest is stream name.
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 || parts[1] == "" {
		return "", "", "", fmt.Errorf("missing stream name in URL %q", rawURL)
	}

	app = parts[0]
	streamName = parts[1] + queryPart
	return host, app, streamName, nil
}

// buildPushReport creates a PushReport from the push session state.
func buildPushReport(cfg PushConfig, start time.Time, framesSent, bytesSent int64) *report.PushReport {
	return &report.PushReport{
		Protocol:   cfg.Protocol,
		Target:     cfg.Target,
		DurationMs: time.Since(start).Milliseconds(),
		FramesSent: framesSent,
		BytesSent:  bytesSent,
	}
}
