package rtmp

import (
	"bytes"
	"log"
	"net"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	flvpkg "github.com/im-pingo/liveforge/pkg/muxer/flv"
)

// Subscriber implements core.Subscriber for RTMP connections.
type Subscriber struct {
	id      string
	conn    net.Conn
	cw      *ChunkWriter
	stream  *core.Stream
	opts    core.SubscribeOptions
	closed  chan struct{}
}

// NewSubscriber creates a new RTMP subscriber.
func NewSubscriber(streamKey string, conn net.Conn, cw *ChunkWriter, stream *core.Stream) *Subscriber {
	return &Subscriber{
		id:     "rtmp-sub-" + streamKey,
		conn:   conn,
		cw:     cw,
		stream: stream,
		opts:   core.DefaultSubscribeOptions(),
		closed: make(chan struct{}),
	}
}

// ID returns the subscriber identifier.
func (s *Subscriber) ID() string { return s.id }

// Options returns the subscribe options.
func (s *Subscriber) Options() core.SubscribeOptions { return s.opts }

// OnData receives muxed data and writes it over the RTMP connection.
func (s *Subscriber) OnData(data []byte) error {
	// data contains an FLV tag — extract and send as RTMP message
	return nil
}

// Close disconnects the subscriber.
func (s *Subscriber) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return s.conn.Close()
}

// WriteLoop reads frames from the stream ring buffer and sends them as RTMP chunks.
func (s *Subscriber) WriteLoop() {
	defer s.Close()

	// Send GOP cache first if in GOP mode
	if s.opts.StartMode == core.StartModeGOP {
		for _, frame := range s.stream.GOPCache() {
			if err := s.sendFrame(frame); err != nil {
				log.Printf("RTMP subscriber %s: GOP cache send error: %v", s.id, err)
				return
			}
		}
	}

	// Read live frames from ring buffer
	reader := s.stream.RingBuffer().NewReader()
	for {
		select {
		case <-s.closed:
			return
		default:
		}

		frame, ok := reader.TryRead()
		if !ok {
			// Use blocking read
			frame, ok = reader.Read()
			if !ok {
				return
			}
		}

		if err := s.sendFrame(frame); err != nil {
			return
		}
	}
}

func (s *Subscriber) sendFrame(frame *avframe.AVFrame) error {
	// Convert AVFrame to RTMP message
	var buf bytes.Buffer
	muxer := flvpkg.NewMuxer()
	if err := muxer.WriteFrame(&buf, frame); err != nil {
		return err
	}

	// Extract the FLV tag data (skip previous tag size at end)
	tagData := buf.Bytes()
	if len(tagData) < flvpkg.TagHeaderSize+4 {
		return nil
	}

	// Send as RTMP message using the original payload format
	var msgTypeID uint8
	if frame.MediaType.IsVideo() {
		msgTypeID = MsgVideo
	} else {
		msgTypeID = MsgAudio
	}

	// Reconstruct the RTMP payload (FLV tag data portion, without tag header)
	dataSize := len(tagData) - flvpkg.TagHeaderSize - 4 // minus header and prev tag size
	if dataSize <= 0 {
		return nil
	}
	payload := tagData[flvpkg.TagHeaderSize : flvpkg.TagHeaderSize+dataSize]

	msg := &Message{
		TypeID:    msgTypeID,
		Length:    uint32(len(payload)),
		Timestamp: uint32(frame.DTS),
		StreamID:  1,
		Payload:   payload,
	}

	return s.cw.WriteMessage(6, msg)
}
