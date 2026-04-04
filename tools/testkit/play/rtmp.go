package play

import (
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
)

const (
	handshakeSize    = 1536
	defaultChunkSize = 4096
	connectTimeout   = 5 * time.Second
)

// rtmpPlayer implements Player for the RTMP protocol.
type rtmpPlayer struct{}

// rtmpConn wraps a net.Conn with RTMP chunk reader/writer.
type rtmpConn struct {
	conn net.Conn
	cr   *rtmp.ChunkReader
	cw   *rtmp.ChunkWriter
}

// Play connects to the target RTMP server, subscribes to the stream, and
// delivers received media frames to onFrame. It respects ctx cancellation and
// cfg.Duration limits.
func (p *rtmpPlayer) Play(ctx context.Context, cfg PlayConfig, onFrame FrameCallback) error {
	host, app, streamName, err := parseRTMPURL(cfg.URL)
	if err != nil {
		return fmt.Errorf("parse RTMP URL: %w", err)
	}

	// Append token to stream name if provided.
	if cfg.Token != "" {
		if strings.Contains(streamName, "?") {
			streamName += "&token=" + cfg.Token
		} else {
			streamName += "?token=" + cfg.Token
		}
	}

	rc, err := dialRTMP(ctx, host)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", host, err)
	}
	defer rc.conn.Close()

	// RTMP command sequence: connect -> read response -> set chunk size ->
	// createStream -> read response -> play -> read play response -> read loop.
	if err := rc.sendConnect(app, host); err != nil {
		return fmt.Errorf("send connect: %w", err)
	}

	// Read connect response (transaction ID 1).
	if err := rc.readResponses(1); err != nil {
		return fmt.Errorf("connect response: %w", err)
	}

	// Set chunk size for better throughput.
	if err := rc.setChunkSize(defaultChunkSize); err != nil {
		return fmt.Errorf("set chunk size: %w", err)
	}

	// createStream (txn 2).
	if err := rc.sendCreateStream(); err != nil {
		return fmt.Errorf("send createStream: %w", err)
	}

	// Read createStream response (transaction ID 2).
	if err := rc.readResponses(2); err != nil {
		return fmt.Errorf("createStream response: %w", err)
	}

	// Send play command on stream ID 1.
	if err := rc.sendPlay(streamName); err != nil {
		return fmt.Errorf("send play: %w", err)
	}

	// Read play response (onStatus).
	if err := rc.readResponses(0); err != nil {
		return fmt.Errorf("play response: %w", err)
	}

	// Determine deadline from cfg.Duration.
	var deadline time.Time
	if cfg.Duration > 0 {
		deadline = time.Now().Add(cfg.Duration)
	}

	// Read loop: receive RTMP messages and deliver media frames.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil
		}

		// Set a read deadline on the connection to avoid blocking forever.
		rc.conn.SetReadDeadline(time.Now().Add(1 * time.Second))

		msg, err := rc.cr.ReadMessage()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				// Timeout — check ctx/duration again.
				continue
			}
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read message: %w", err)
		}

		switch msg.TypeID {
		case rtmp.MsgSetChunkSize:
			if len(msg.Payload) >= 4 {
				size := int(binary.BigEndian.Uint32(msg.Payload))
				rc.cr.SetChunkSize(size)
			}
		case rtmp.MsgVideo:
			frame := parseVideoPayload(msg.Payload, int64(msg.Timestamp))
			if frame != nil {
				onFrame(frame)
			}
		case rtmp.MsgAudio:
			frame := parseAudioPayload(msg.Payload, int64(msg.Timestamp))
			if frame != nil {
				onFrame(frame)
			}
		case rtmp.MsgWindowAckSize, rtmp.MsgSetPeerBandwidth, rtmp.MsgAck, rtmp.MsgUserControl:
			// Protocol control — ignore.
		case rtmp.MsgAMF0Command:
			// Server may send onStatus or other commands — ignore.
		}
	}
}

// dialRTMP connects to an RTMP server, performs the handshake, and returns an
// initialized rtmpConn.
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
	if _, err := rand.Read(c0c1[1:]); err != nil {
		return fmt.Errorf("generate random C1: %w", err)
	}

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

// sendConnect sends the RTMP connect command (transaction ID 1).
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

// sendCreateStream sends the RTMP createStream command (transaction ID 2).
func (rc *rtmpConn) sendCreateStream() error {
	payload, _ := rtmp.AMF0Encode("createStream", float64(2), nil)
	return rc.cw.WriteMessage(3, &rtmp.Message{
		TypeID:  rtmp.MsgAMF0Command,
		Length:  uint32(len(payload)),
		Payload: payload,
	})
}

// sendPlay sends the RTMP play command on stream ID 1.
func (rc *rtmpConn) sendPlay(streamName string) error {
	payload, _ := rtmp.AMF0Encode("play", float64(0), nil, streamName)
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
	if err := rc.cw.WriteMessage(2, &rtmp.Message{
		TypeID:  rtmp.MsgSetChunkSize,
		Length:  4,
		Payload: payload,
	}); err != nil {
		return err
	}
	rc.cw.SetChunkSize(size)
	return nil
}

// parseVideoPayload parses an FLV video tag body into an AVFrame.
func parseVideoPayload(data []byte, dts int64) *avframe.AVFrame {
	if len(data) < 5 {
		return nil
	}

	frameTypeID := (data[0] >> 4) & 0x0F
	codecID := data[0] & 0x0F

	codec := flvpkg.FLVVideoCodecToAVFrame(codecID)
	if codec == 0 {
		return nil
	}

	avcPacketType := data[1]

	var frameType avframe.FrameType
	if avcPacketType == flvpkg.AVCPacketSequenceHeader {
		frameType = avframe.FrameTypeSequenceHeader
	} else if frameTypeID == flvpkg.VideoFrameKeyframe {
		frameType = avframe.FrameTypeKeyframe
	} else {
		frameType = avframe.FrameTypeInterframe
	}

	cts := int64(int32(binary.BigEndian.Uint32([]byte{0, data[2], data[3], data[4]})) >> 8)

	return avframe.NewAVFrame(avframe.MediaTypeVideo, codec, frameType, dts, dts+cts, data[5:])
}

// parseAudioPayload parses an FLV audio tag body into an AVFrame.
func parseAudioPayload(data []byte, dts int64) *avframe.AVFrame {
	if len(data) < 2 {
		return nil
	}

	formatID := (data[0] >> 4) & 0x0F
	codec := flvpkg.FLVAudioCodecToAVFrame(formatID)
	if codec == 0 {
		return nil
	}

	var frameType avframe.FrameType
	if codec == avframe.CodecAAC && data[1] == flvpkg.AACPacketSequenceHeader {
		frameType = avframe.FrameTypeSequenceHeader
	} else {
		frameType = avframe.FrameTypeInterframe
	}

	return avframe.NewAVFrame(avframe.MediaTypeAudio, codec, frameType, dts, dts, data[2:])
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

	// Strip query string from path parsing; token is handled via PlayConfig.Token.
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
