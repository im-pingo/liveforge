// module/cluster/transport_rtmp.go
package cluster

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/module/rtmp"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

// RTMPTransport implements RelayTransport for RTMP protocol.
type RTMPTransport struct{}

// NewRTMPTransport creates a new RTMP relay transport.
func NewRTMPTransport() *RTMPTransport {
	return &RTMPTransport{}
}

func (t *RTMPTransport) Scheme() string { return "rtmp" }

func (t *RTMPTransport) Push(ctx context.Context, targetURL string, stream *core.Stream) error {
	host, app, streamName, err := parseRTMPURL(targetURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}

	rc, err := dialRTMP(host)
	if err != nil {
		return err
	}
	defer rc.conn.Close()

	if err := rc.setChunkSize(defaultChunkSize); err != nil {
		return fmt.Errorf("set chunk size: %w", err)
	}
	if err := rc.sendConnect(app); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if err := rc.readResponses(1); err != nil {
		return fmt.Errorf("connect response: %w", err)
	}
	if err := rc.sendPublish(streamName); err != nil {
		return fmt.Errorf("publish commands: %w", err)
	}
	if err := rc.readResponses(4); err != nil {
		return fmt.Errorf("createStream response: %w", err)
	}

	publishPayload, _ := rtmp.AMF0Encode("publish", float64(5), nil, streamName, "live")
	if err := rc.cw.WriteMessage(8, &rtmp.Message{
		TypeID: rtmp.MsgAMF0Command, Length: uint32(len(publishPayload)),
		StreamID: 1, Payload: publishPayload,
	}); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	if err := rc.readResponses(5); err != nil {
		return fmt.Errorf("publish response: %w", err)
	}

	slog.Info("rtmp relay push connected", "module", "cluster", "target", targetURL)

	if vsh := stream.VideoSeqHeader(); vsh != nil {
		if err := rc.sendMediaFrame(vsh); err != nil {
			return fmt.Errorf("video seq header: %w", err)
		}
	}
	if ash := stream.AudioSeqHeader(); ash != nil {
		if err := rc.sendMediaFrame(ash); err != nil {
			return fmt.Errorf("audio seq header: %w", err)
		}
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

		if err := rc.sendMediaFrame(frame); err != nil {
			return fmt.Errorf("send frame: %w", err)
		}
	}
}

func (t *RTMPTransport) Pull(ctx context.Context, sourceURL string, stream *core.Stream) error {
	host, app, streamName, err := parseRTMPURL(sourceURL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}

	rc, err := dialRTMP(host)
	if err != nil {
		return err
	}
	defer rc.conn.Close()

	if err := rc.sendConnect(app); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if err := rc.readResponses(1); err != nil {
		return fmt.Errorf("connect response: %w", err)
	}
	if err := rc.sendPlay(streamName); err != nil {
		return fmt.Errorf("play commands: %w", err)
	}
	if err := rc.readResponses(2); err != nil {
		return fmt.Errorf("createStream response: %w", err)
	}

	playPayload, _ := rtmp.AMF0Encode("play", float64(3), nil, streamName)
	if err := rc.cw.WriteMessage(8, &rtmp.Message{
		TypeID:   rtmp.MsgAMF0Command,
		Length:   uint32(len(playPayload)),
		StreamID: 1,
		Payload:  playPayload,
	}); err != nil {
		return fmt.Errorf("play: %w", err)
	}

	slog.Info("rtmp relay pull connected", "module", "cluster", "source", sourceURL)

	pub := &originPublisher{
		id:   fmt.Sprintf("rtmp-pull-%s", stream.Key()),
		info: &avframe.MediaInfo{},
	}
	if err := stream.SetPublisher(pub); err != nil {
		return fmt.Errorf("set publisher: %w", err)
	}
	defer stream.RemovePublisher()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		msg, err := rc.cr.ReadMessage()
		if err != nil {
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
				if pub.info.VideoCodec == 0 {
					pub.info.VideoCodec = frame.Codec
				}
				stream.WriteFrame(frame)
			}
		case rtmp.MsgAudio:
			frame := parseAudioPayload(msg.Payload, int64(msg.Timestamp))
			if frame != nil {
				if pub.info.AudioCodec == 0 {
					pub.info.AudioCodec = frame.Codec
				}
				stream.WriteFrame(frame)
			}
		case rtmp.MsgAMF0Command:
			vals, err := rtmp.AMF0Decode(msg.Payload)
			if err != nil || len(vals) < 1 {
				continue
			}
			cmd, _ := vals[0].(string)
			if cmd == "onStatus" && len(vals) >= 4 {
				if m, ok := vals[3].(map[string]any); ok {
					code, _ := m["code"].(string)
					if code == "NetStream.Play.UnpublishNotify" || code == "NetStream.Play.Stop" {
						slog.Info("rtmp relay stream ended", "module", "cluster", "code", code)
						return nil
					}
				}
			}
		}
	}
}

func (t *RTMPTransport) Close() error { return nil }
