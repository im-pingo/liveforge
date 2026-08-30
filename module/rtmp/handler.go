package rtmp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/aac"
)

// isConnClosed checks if an error indicates a closed connection.
func isConnClosed(err error) bool {
	if err == nil {
		return false
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return strings.Contains(netErr.Err.Error(), "use of closed")
	}
	return strings.Contains(err.Error(), "use of closed network connection")
}

// Handler manages a single RTMP connection.
type Handler struct {
	conn     net.Conn
	cr       *ChunkReader
	cw       *ChunkWriter
	hub      *core.StreamHub
	eventBus *core.EventBus

	app                 string
	streamKey           string
	isPublisher         bool
	publisher           *Publisher
	streamInstanceID    uint64
	publisherGeneration uint64
	publishCleanupOnce  sync.Once
	appParams           map[string]string // params from connect (app field query string)
	caps                PeerCapabilities

	chunkSize int
	skipCfg   *config.SkipTrackerConfig
}

// NewHandler creates a new RTMP connection handler.
func NewHandler(conn net.Conn, hub *core.StreamHub, eventBus *core.EventBus, chunkSize int, skipCfg *config.SkipTrackerConfig) *Handler {
	return &Handler{
		conn:      conn,
		cr:        NewChunkReader(conn, DefaultChunkSize),
		cw:        NewChunkWriter(conn, DefaultChunkSize),
		hub:       hub,
		eventBus:  eventBus,
		chunkSize: chunkSize,
		skipCfg:   skipCfg,
	}
}

// Handle processes the RTMP connection after handshake.
func (h *Handler) Handle() error {
	defer h.conn.Close()
	defer h.cleanup()

	for {
		msg, err := h.cr.ReadMessage()
		if err != nil {
			if err == io.EOF || isConnClosed(err) {
				return nil
			}
			return fmt.Errorf("read message: %w", err)
		}

		if err := h.handleMessage(msg); err != nil {
			return err
		}
	}
}

// cleanup releases the publisher when the connection closes for any reason.
func (h *Handler) cleanup() {
	h.publishCleanupOnce.Do(func() {
		if h.streamKey == "" || !h.isPublisher {
			return
		}
		if stream, ok := h.hub.Find(h.streamKey); ok {
			stream.RemovePublisherIf(h.publisher)
		}
		publisherID := ""
		if h.publisher != nil {
			publisherID = h.publisher.ID()
		}
		if err := h.eventBus.EmitAsync(core.EventPublishStop, &core.EventContext{
			StreamKey:           h.streamKey,
			StreamInstanceID:    h.streamInstanceID,
			PublisherGeneration: h.publisherGeneration,
			PublisherID:         publisherID,
			Protocol:            "rtmp",
			RemoteAddr:          h.conn.RemoteAddr().String(),
		}); err != nil {
			slog.Error("publisher terminal lifecycle admission failed", "module", "rtmp", "stream", h.streamKey, "error", err)
		}
	})
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
			h.caps = ParsePeerCapabilities(obj)
			if app, ok := obj["app"].(string); ok {
				// Parse query string from app field (e.g. "live?token=xxx")
				if parts := strings.SplitN(app, "?", 2); len(parts) == 2 {
					h.app = parts[0]
					h.appParams = parseQueryParams(parts[1])
				} else {
					h.app = app
				}
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
	var mergedParams map[string]string
	if len(vals) >= 4 {
		if name, ok := vals[3].(string); ok {
			// Parse query string from stream name (e.g. "test?token=xxx")
			cleanName, params := splitNameParams(name)
			h.streamKey = h.app + "/" + cleanName
			// Merge app-level params with stream-level params (stream-level wins)
			mergedParams = mergeParams(h.appParams, params)
		}
	}
	if h.streamKey == "" {
		return fmt.Errorf("publish: missing stream name")
	}

	pub := NewPublisher(h.streamKey, h.conn)
	publishCtx := &core.EventContext{
		StreamKey:   h.streamKey,
		PublisherID: pub.ID(),
		Protocol:    "rtmp",
		RemoteAddr:  h.conn.RemoteAddr().String(),
		Params:      mergedParams,
	}
	if err := h.eventBus.EmitSync(core.EventPublish, publishCtx); err != nil {
		_ = h.sendOnStatus("error", "NetStream.Publish.Rejected", err.Error())
		return fmt.Errorf("publish %s: %w", h.streamKey, err)
	}
	stream, err := h.hub.GetOrCreate(h.streamKey)
	if err != nil {
		_ = h.sendOnStatus("error", "NetStream.Publish.Rejected", err.Error())
		return fmt.Errorf("publish %s: %w", h.streamKey, err)
	}
	if err := stream.SetPublisher(pub); err != nil {
		return fmt.Errorf("publish %s: %w", h.streamKey, err)
	}
	startup := stream.StartupSnapshot()
	publishCtx.StreamInstanceID = startup.StreamInstanceID
	publishCtx.PublisherGeneration = startup.Generation
	if err := h.eventBus.EmitAsync(core.EventPublish, publishCtx); err != nil {
		stream.RemovePublisherIf(pub)
		_ = h.sendOnStatus("error", "NetStream.Publish.Rejected", err.Error())
		return fmt.Errorf("publish %s: %w", h.streamKey, err)
	}
	h.publisher = pub
	h.isPublisher = true
	h.streamInstanceID = startup.StreamInstanceID
	h.publisherGeneration = startup.Generation

	// Send onStatus(NetStream.Publish.Start)
	return h.sendOnStatus("status", "NetStream.Publish.Start", "Publishing started")
}

func (h *Handler) onPlay(vals []any) error {
	if len(vals) >= 4 {
		if name, ok := vals[3].(string); ok {
			// Parse query string from stream name
			cleanName, params := splitNameParams(name)
			h.streamKey = h.app + "/" + cleanName
			mergedParams := mergeParams(h.appParams, params)

			// Authorization is synchronous and independent from lifecycle start.
			if err := h.eventBus.EmitSync(core.EventSubscribe, &core.EventContext{
				StreamKey:  h.streamKey,
				Protocol:   "rtmp",
				RemoteAddr: h.conn.RemoteAddr().String(),
				Params:     mergedParams,
			}); err != nil {
				_ = h.sendOnStatus("error", "NetStream.Play.Rejected", err.Error())
				return fmt.Errorf("play %s: %w", h.streamKey, err)
			}
		}
	}
	if h.streamKey == "" {
		return fmt.Errorf("play: missing stream name")
	}

	// Send StreamBegin user control message
	if err := h.sendStreamBegin(1); err != nil {
		return err
	}

	// Send onStatus(NetStream.Play.Start)
	if err := h.sendOnStatus("status", "NetStream.Play.Start", "Playback started"); err != nil {
		return err
	}

	// Start subscriber write loop
	stream, err := h.hub.GetOrCreate(h.streamKey)
	if err != nil {
		_ = h.sendOnStatus("error", "NetStream.Play.Rejected", err.Error())
		return fmt.Errorf("play %s: %w", h.streamKey, err)
	}
	startup := stream.StartupSnapshot()
	releaseSubscriber, err := stream.AddSubscriberForGeneration("rtmp", startup.Generation)
	if err != nil {
		_ = h.sendOnStatus("error", "NetStream.Play.Rejected", err.Error())
		return fmt.Errorf("play %s: %w", h.streamKey, err)
	}
	sub := NewSubscriberWithCapabilities(h.streamKey, h.conn, h.cw, stream, h.skipCfg, h.caps, func(err error) {
		slog.Warn("rtmp subscriber cannot represent stream codec", "subscriber", "rtmp-sub-"+h.streamKey, "error", err)
	})
	sub.startup = &startup
	lifecycleCtx := &core.EventContext{
		StreamKey:           h.streamKey,
		StreamInstanceID:    startup.StreamInstanceID,
		PublisherGeneration: startup.Generation,
		PublisherID:         startup.PublisherID,
		SubscriberID:        sub.ID(),
		Protocol:            "rtmp",
		RemoteAddr:          h.conn.RemoteAddr().String(),
	}
	if err := h.eventBus.EmitAsync(core.EventSubscribe, lifecycleCtx); err != nil {
		releaseSubscriber()
		_ = sub.Close()
		return fmt.Errorf("play %s: %w", h.streamKey, err)
	}
	go func() {
		defer func() {
			releaseSubscriber()
			if err := h.eventBus.EmitAsync(core.EventSubscribeStop, lifecycleCtx); err != nil {
				slog.Error("subscriber terminal lifecycle admission failed", "module", "rtmp", "stream", h.streamKey, "error", err)
			}
		}()
		sub.WriteLoop()
	}()

	return nil
}

func (h *Handler) onDeleteStream() error {
	// Cleanup is handled by the deferred cleanup() call in Handle().
	// Clear streamKey so cleanup knows this was a graceful disconnect
	// and the publisher was already logically removed.
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
	var (
		frame    *avframe.AVFrame
		parseErr error
	)

	if msg.TypeID == MsgVideo {
		frame, parseErr = parseVideoPayloadWithError(msg.Payload, int64(msg.Timestamp))
	} else {
		frame, parseErr = parseAudioPayloadWithError(msg.Payload, int64(msg.Timestamp))
	}
	if parseErr != nil {
		slog.Debug("drop invalid RTMP media payload", "type", msg.TypeID, "error", parseErr)
		return nil
	}

	if frame != nil {
		// Update publisher's MediaInfo when sequence headers arrive.
		// Copy-on-write: build a new snapshot and publish it atomically so
		// concurrent readers (RTSP/WebRTC/HTTP subscribers) never observe a
		// partially updated struct.
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			if rp := h.publisher; rp != nil {
				mi := *rp.MediaInfo()
				if frame.MediaType.IsVideo() {
					mi.VideoCodec = frame.Codec
					mi.VideoSequenceHeader = append([]byte(nil), frame.Payload...)
				} else if frame.MediaType.IsAudio() {
					mi.AudioCodec = frame.Codec
					mi.AudioSequenceHeader = append([]byte(nil), frame.Payload...)
					if frame.Codec == avframe.CodecAAC && len(frame.Payload) >= 2 {
						if ascInfo, err := aac.ParseAudioSpecificConfig(frame.Payload); err == nil {
							mi.SampleRate = ascInfo.SampleRate
							mi.Channels = ascInfo.Channels
						}
					}
				}
				rp.SetMediaInfo(&mi)
			}
		}
		if !stream.WriteFrameForPublisher(h.publisher, frame) && stream.Publisher() != h.publisher {
			return fmt.Errorf("RTMP publisher no longer owns stream %s", h.streamKey)
		}
	}
	return nil
}
