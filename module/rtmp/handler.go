package rtmp

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strings"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	flvpkg "github.com/im-pingo/liveforge/pkg/muxer/flv"
)

// Handler manages a single RTMP connection.
type Handler struct {
	conn     net.Conn
	cr       *ChunkReader
	cw       *ChunkWriter
	hub      *core.StreamHub
	eventBus *core.EventBus

	app       string
	streamKey string

	chunkSize int
}

// NewHandler creates a new RTMP connection handler.
func NewHandler(conn net.Conn, hub *core.StreamHub, eventBus *core.EventBus, chunkSize int) *Handler {
	return &Handler{
		conn:      conn,
		cr:        NewChunkReader(conn, DefaultChunkSize),
		cw:        NewChunkWriter(conn, DefaultChunkSize),
		hub:       hub,
		eventBus:  eventBus,
		chunkSize: chunkSize,
	}
}

// Handle processes the RTMP connection after handshake.
func (h *Handler) Handle() error {
	defer h.conn.Close()

	for {
		msg, err := h.cr.ReadMessage()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read message: %w", err)
		}

		if err := h.handleMessage(msg); err != nil {
			return err
		}
	}
}

func (h *Handler) handleMessage(msg *Message) error {
	switch msg.TypeID {
	case MsgSetChunkSize:
		if len(msg.Payload) < 4 {
			return fmt.Errorf("invalid SetChunkSize payload")
		}
		size := int(binary.BigEndian.Uint32(msg.Payload))
		h.cr.SetChunkSize(size)
		return nil

	case MsgWindowAckSize:
		// Acknowledge, no action needed
		return nil

	case MsgAMF0Command:
		return h.handleCommand(msg)

	case MsgAMF0Data:
		// Metadata, skip for now
		return nil

	case MsgVideo, MsgAudio:
		return h.handleMediaMessage(msg)

	default:
		// Ignore unknown messages
		return nil
	}
}

func (h *Handler) handleCommand(msg *Message) error {
	vals, err := AMF0Decode(msg.Payload)
	if err != nil {
		return fmt.Errorf("AMF0 decode: %w", err)
	}
	if len(vals) < 1 {
		return nil
	}

	cmd, _ := vals[0].(string)
	switch cmd {
	case "connect":
		return h.onConnect(vals)
	case "createStream":
		return h.onCreateStream(vals)
	case "publish":
		return h.onPublish(vals)
	case "play":
		return h.onPlay(vals)
	case "deleteStream":
		return h.onDeleteStream()
	case "FCPublish", "releaseStream", "FCUnpublish":
		// Respond with _result silently
		return nil
	default:
		return nil
	}
}

func (h *Handler) onConnect(vals []any) error {
	// Extract app name from command object
	if len(vals) >= 3 {
		if obj, ok := vals[2].(map[string]any); ok {
			if app, ok := obj["app"].(string); ok {
				h.app = app
			}
		}
	}

	// Send Window Acknowledgement Size
	if err := h.sendSetWindowAckSize(2500000); err != nil {
		return err
	}

	// Send Set Peer Bandwidth
	if err := h.sendSetPeerBandwidth(2500000, 2); err != nil {
		return err
	}

	// Send Set Chunk Size
	if err := h.sendSetChunkSize(h.chunkSize); err != nil {
		return err
	}

	// Send _result
	txID := float64(1)
	if len(vals) >= 2 {
		if id, ok := vals[1].(float64); ok {
			txID = id
		}
	}

	return h.sendConnectResult(txID)
}

func (h *Handler) onCreateStream(vals []any) error {
	txID := float64(2)
	if len(vals) >= 2 {
		if id, ok := vals[1].(float64); ok {
			txID = id
		}
	}

	payload, err := AMF0Encode("_result", txID, nil, float64(1))
	if err != nil {
		return err
	}
	return h.cw.WriteMessage(3, &Message{
		TypeID:  MsgAMF0Command,
		Length:  uint32(len(payload)),
		Payload: payload,
	})
}

func (h *Handler) onPublish(vals []any) error {
	if len(vals) >= 4 {
		if name, ok := vals[3].(string); ok {
			h.streamKey = h.app + "/" + name
		}
	}
	if h.streamKey == "" {
		return fmt.Errorf("publish: missing stream name")
	}

	stream := h.hub.GetOrCreate(h.streamKey)
	pub := NewPublisher(h.streamKey, h.conn)
	if err := stream.SetPublisher(pub); err != nil {
		return fmt.Errorf("publish %s: %w", h.streamKey, err)
	}

	// Emit publish event
	h.eventBus.Emit(core.EventPublish, &core.EventContext{
		StreamKey:  h.streamKey,
		Protocol:   "rtmp",
		RemoteAddr: h.conn.RemoteAddr().String(),
	})

	// Send onStatus(NetStream.Publish.Start)
	return h.sendOnStatus("status", "NetStream.Publish.Start", "Publishing started")
}

func (h *Handler) onPlay(vals []any) error {
	if len(vals) >= 4 {
		if name, ok := vals[3].(string); ok {
			// Remove query string if present
			name = strings.SplitN(name, "?", 2)[0]
			h.streamKey = h.app + "/" + name
		}
	}
	if h.streamKey == "" {
		return fmt.Errorf("play: missing stream name")
	}

	// Emit subscribe event
	h.eventBus.Emit(core.EventSubscribe, &core.EventContext{
		StreamKey:  h.streamKey,
		Protocol:   "rtmp",
		RemoteAddr: h.conn.RemoteAddr().String(),
	})

	// Send onStatus(NetStream.Play.Start)
	if err := h.sendOnStatus("status", "NetStream.Play.Start", "Playback started"); err != nil {
		return err
	}

	// Start subscriber write loop
	stream := h.hub.GetOrCreate(h.streamKey)
	sub := NewSubscriber(h.streamKey, h.conn, h.cw, stream)
	go sub.WriteLoop()

	return nil
}

func (h *Handler) onDeleteStream() error {
	if h.streamKey != "" {
		if stream, ok := h.hub.Find(h.streamKey); ok {
			stream.RemovePublisher()
		}
	}
	return nil
}

func (h *Handler) handleMediaMessage(msg *Message) error {
	if h.streamKey == "" {
		return nil
	}

	stream, ok := h.hub.Find(h.streamKey)
	if !ok {
		return nil
	}

	// Convert RTMP message to AVFrame using FLV parsing
	// RTMP video/audio payloads use the same format as FLV tag data
	var frame *avframe.AVFrame

	if msg.TypeID == MsgVideo {
		frame = parseVideoPayload(msg.Payload, int64(msg.Timestamp))
	} else {
		frame = parseAudioPayload(msg.Payload, int64(msg.Timestamp))
	}

	if frame != nil {
		stream.WriteFrame(frame)
	}
	return nil
}

func parseVideoPayload(data []byte, dts int64) *avframe.AVFrame {
	if len(data) < 5 {
		return nil
	}

	frameTypeID := (data[0] >> 4) & 0x0F
	codecID := data[0] & 0x0F
	avcPacketType := data[1]
	cts := int64(int32(binary.BigEndian.Uint32([]byte{0, data[2], data[3], data[4]})) >> 8)

	codec := flvpkg.FLVVideoCodecToAVFrame(codecID)
	if codec == 0 {
		return nil
	}

	var frameType avframe.FrameType
	if avcPacketType == flvpkg.AVCPacketSequenceHeader {
		frameType = avframe.FrameTypeSequenceHeader
	} else if frameTypeID == flvpkg.VideoFrameKeyframe {
		frameType = avframe.FrameTypeKeyframe
	} else {
		frameType = avframe.FrameTypeInterframe
	}

	return avframe.NewAVFrame(avframe.MediaTypeVideo, codec, frameType, dts, dts+cts, data[5:])
}

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

// Protocol messages

func (h *Handler) sendSetWindowAckSize(size uint32) error {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, size)
	return h.cw.WriteMessage(2, &Message{TypeID: MsgWindowAckSize, Length: 4, Payload: payload})
}

func (h *Handler) sendSetPeerBandwidth(size uint32, limitType byte) error {
	payload := make([]byte, 5)
	binary.BigEndian.PutUint32(payload, size)
	payload[4] = limitType
	return h.cw.WriteMessage(2, &Message{TypeID: MsgSetPeerBandwidth, Length: 5, Payload: payload})
}

func (h *Handler) sendSetChunkSize(size int) error {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(size))
	if err := h.cw.WriteMessage(2, &Message{TypeID: MsgSetChunkSize, Length: 4, Payload: payload}); err != nil {
		return err
	}
	h.cw.SetChunkSize(size)
	return nil
}

func (h *Handler) sendConnectResult(txID float64) error {
	payload, err := AMF0Encode(
		"_result",
		txID,
		map[string]any{
			"fmsVer":       "FMS/3,5,7,7009",
			"capabilities": float64(31),
		},
		map[string]any{
			"level":       "status",
			"code":        "NetConnection.Connect.Success",
			"description": "Connection succeeded",
		},
	)
	if err != nil {
		return err
	}
	return h.cw.WriteMessage(3, &Message{TypeID: MsgAMF0Command, Length: uint32(len(payload)), Payload: payload})
}

func (h *Handler) sendOnStatus(level, code, description string) error {
	payload, err := AMF0Encode(
		"onStatus",
		float64(0),
		nil,
		map[string]any{
			"level":       level,
			"code":        code,
			"description": description,
		},
	)
	if err != nil {
		return err
	}
	return h.cw.WriteMessage(5, &Message{TypeID: MsgAMF0Command, Length: uint32(len(payload)), Payload: payload, StreamID: 1})
}

// logError logs errors that occur during connection handling.
func logError(msg string, err error) {
	if err != nil {
		log.Printf("RTMP: %s: %v", msg, err)
	}
}
