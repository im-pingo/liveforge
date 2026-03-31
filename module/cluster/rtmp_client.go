package cluster

import (
	"bytes"
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

// rtmpConn wraps a net.Conn with RTMP chunk reader/writer.
type rtmpConn struct {
	conn net.Conn
	cr   *rtmp.ChunkReader
	cw   *rtmp.ChunkWriter
}

// dialRTMP connects to an RTMP server, performs the handshake,
// and returns an initialized rtmpConn.
func dialRTMP(addr string) (*rtmpConn, error) {
	conn, err := net.DialTimeout("tcp", addr, connectTimeout)
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
	// Send C0 (version 3) + C1 (1536 random bytes)
	c0c1 := make([]byte, 1+handshakeSize)
	c0c1[0] = 3
	rand.Read(c0c1[1:])

	if _, err := conn.Write(c0c1); err != nil {
		return fmt.Errorf("write C0C1: %w", err)
	}

	// Read S0 + S1 + S2
	s0s1s2 := make([]byte, 1+handshakeSize+handshakeSize)
	if _, err := io.ReadFull(conn, s0s1s2); err != nil {
		return fmt.Errorf("read S0S1S2: %w", err)
	}

	// Send C2 (echo S1)
	c2 := s0s1s2[1 : 1+handshakeSize]
	if _, err := conn.Write(c2); err != nil {
		return fmt.Errorf("write C2: %w", err)
	}

	return nil
}

// sendConnect sends RTMP connect command.
func (rc *rtmpConn) sendConnect(app string) error {
	payload, err := rtmp.AMF0Encode(
		"connect",
		float64(1),
		map[string]any{
			"app":      app,
			"type":     "nonprivate",
			"flashVer": "FMLE/3.0",
			"tcUrl":    "rtmp://localhost/" + app,
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

// sendPublish sends RTMP publish command sequence (releaseStream, FCPublish, createStream, publish).
func (rc *rtmpConn) sendPublish(streamName string) error {
	// releaseStream
	payload, _ := rtmp.AMF0Encode("releaseStream", float64(2), nil, streamName)
	if err := rc.cw.WriteMessage(3, &rtmp.Message{
		TypeID: rtmp.MsgAMF0Command, Length: uint32(len(payload)), Payload: payload,
	}); err != nil {
		return err
	}

	// FCPublish
	payload, _ = rtmp.AMF0Encode("FCPublish", float64(3), nil, streamName)
	if err := rc.cw.WriteMessage(3, &rtmp.Message{
		TypeID: rtmp.MsgAMF0Command, Length: uint32(len(payload)), Payload: payload,
	}); err != nil {
		return err
	}

	// createStream
	payload, _ = rtmp.AMF0Encode("createStream", float64(4), nil)
	if err := rc.cw.WriteMessage(3, &rtmp.Message{
		TypeID: rtmp.MsgAMF0Command, Length: uint32(len(payload)), Payload: payload,
	}); err != nil {
		return err
	}

	return nil
}

// sendPlay sends RTMP play command sequence (createStream, play).
func (rc *rtmpConn) sendPlay(streamName string) error {
	// createStream
	payload, _ := rtmp.AMF0Encode("createStream", float64(2), nil)
	if err := rc.cw.WriteMessage(3, &rtmp.Message{
		TypeID: rtmp.MsgAMF0Command, Length: uint32(len(payload)), Payload: payload,
	}); err != nil {
		return err
	}

	return nil
}

// readResponses reads and processes RTMP response messages until we get
// a result for the given transaction ID or encounter an error.
func (rc *rtmpConn) readResponses(targetTxnID float64) error {
	for i := 0; i < 20; i++ {
		msg, err := rc.cr.ReadMessage()
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}

		// Handle protocol control messages
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
func (rc *rtmpConn) sendMediaFrame(frame *avframe.AVFrame) error {
	payload, err := buildRTMPPayload(frame)
	if err != nil || payload == nil {
		return err
	}

	var msgTypeID uint8
	if frame.MediaType.IsVideo() {
		msgTypeID = rtmp.MsgVideo
	} else {
		msgTypeID = rtmp.MsgAudio
	}

	return rc.cw.WriteMessage(6, &rtmp.Message{
		TypeID:    msgTypeID,
		Length:    uint32(len(payload)),
		Timestamp: uint32(frame.DTS),
		StreamID:  1,
		Payload:   payload,
	})
}

// buildRTMPPayload converts an AVFrame to RTMP FLV tag body (without tag header).
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

// parseRTMPURL parses an RTMP URL into host, app, and stream name.
// Format: rtmp://host[:port]/app[/instance]/streamName
func parseRTMPURL(rawURL string) (host, app, streamName string, err error) {
	// Strip scheme
	u := rawURL
	if strings.HasPrefix(u, "rtmp://") {
		u = u[len("rtmp://"):]
	} else {
		return "", "", "", fmt.Errorf("unsupported scheme in %q", rawURL)
	}

	// Split host from path
	slashIdx := strings.Index(u, "/")
	if slashIdx < 0 {
		return "", "", "", fmt.Errorf("no path in URL %q", rawURL)
	}
	host = u[:slashIdx]
	path := u[slashIdx+1:]

	// Default port
	if !strings.Contains(host, ":") {
		host += ":1935"
	}

	// Split app and stream name: first segment is app, rest is stream name
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 || parts[1] == "" {
		return "", "", "", fmt.Errorf("missing stream name in URL %q", rawURL)
	}

	app = parts[0]
	streamName = parts[1]
	return host, app, streamName, nil
}

// parseVideoPayload parses FLV video tag body into an AVFrame.
func parseVideoPayload(data []byte, dts int64) *avframe.AVFrame {
	if len(data) < 5 {
		return nil
	}

	frameType := (data[0] >> 4) & 0x0F
	codecID := data[0] & 0x0F

	codec := flvpkg.FLVVideoCodecToAVFrame(codecID)
	if codec == 0 {
		return nil
	}

	ft := avframe.FrameTypeInterframe
	if frameType == flvpkg.VideoFrameKeyframe {
		if data[1] == flvpkg.AVCPacketSequenceHeader {
			ft = avframe.FrameTypeSequenceHeader
		} else {
			ft = avframe.FrameTypeKeyframe
		}
	}

	cts := int64(data[2])<<16 | int64(data[3])<<8 | int64(data[4])

	return &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     codec,
		FrameType: ft,
		DTS:       dts,
		PTS:       dts + cts,
		Payload:   data[5:],
	}
}

// parseAudioPayload parses FLV audio tag body into an AVFrame.
func parseAudioPayload(data []byte, dts int64) *avframe.AVFrame {
	if len(data) < 2 {
		return nil
	}

	formatID := (data[0] >> 4) & 0x0F
	codec := flvpkg.FLVAudioCodecToAVFrame(formatID)
	if codec == 0 {
		return nil
	}

	ft := avframe.FrameTypeInterframe
	if codec == avframe.CodecAAC && data[1] == flvpkg.AACPacketSequenceHeader {
		ft = avframe.FrameTypeSequenceHeader
	}

	return &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     codec,
		FrameType: ft,
		DTS:       dts,
		PTS:       dts,
		Payload:   data[2:],
	}
}

