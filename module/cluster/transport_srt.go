package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	gosrt "github.com/datarhei/gosrt"
	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

// SRTTransport implements RelayTransport for SRT protocol.
type SRTTransport struct {
	cfg config.ClusterSRTConfig
}

// NewSRTTransport creates a new SRT relay transport.
func NewSRTTransport(cfg config.ClusterSRTConfig) *SRTTransport {
	return &SRTTransport{cfg: cfg}
}

func (t *SRTTransport) Scheme() string { return "srt" }

func (t *SRTTransport) Push(ctx context.Context, targetURL string, stream *core.Stream) error {
	addr, streamID, err := parseSRTURL(targetURL)
	if err != nil {
		return fmt.Errorf("parse SRT URL: %w", err)
	}

	srtCfg := t.buildConfig()
	srtCfg.StreamId = "publish:" + streamID

	conn, err := gosrt.Dial("srt", addr, srtCfg)
	if err != nil {
		return fmt.Errorf("srt dial %s: %w", addr, err)
	}
	defer conn.Close()

	slog.Info("srt relay push connected", "module", "cluster", "target", targetURL)

	pub := stream.Publisher()
	if pub == nil {
		return fmt.Errorf("stream has no publisher")
	}
	mi := pub.MediaInfo()

	var videoSeqData, audioSeqData []byte
	if vsh := stream.VideoSeqHeader(); vsh != nil {
		videoSeqData = vsh.Payload
	}
	if ash := stream.AudioSeqHeader(); ash != nil {
		audioSeqData = ash.Payload
	}

	muxer := ts.NewMuxer(mi.VideoCodec, mi.AudioCodec, videoSeqData, audioSeqData)

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

		data := muxer.WriteFrame(frame)
		if len(data) == 0 {
			continue
		}

		if _, err := conn.Write(data); err != nil {
			return fmt.Errorf("srt write: %w", err)
		}
	}
}

func (t *SRTTransport) Pull(ctx context.Context, sourceURL string, stream *core.Stream) error {
	addr, streamID, err := parseSRTURL(sourceURL)
	if err != nil {
		return fmt.Errorf("parse SRT URL: %w", err)
	}

	srtCfg := t.buildConfig()
	srtCfg.StreamId = "subscribe:" + streamID

	conn, err := gosrt.Dial("srt", addr, srtCfg)
	if err != nil {
		return fmt.Errorf("srt dial %s: %w", addr, err)
	}

	// gosrt SetReadDeadline is a no-op, so close the connection on context cancel
	// to unblock any blocking Read call.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	slog.Info("srt relay pull connected", "module", "cluster", "source", sourceURL)

	pub := &originPublisher{
		id:   fmt.Sprintf("srt-pull-%s", stream.Key()),
		info: &avframe.MediaInfo{},
	}
	if err := stream.SetPublisher(pub); err != nil {
		return fmt.Errorf("set publisher: %w", err)
	}
	defer stream.RemovePublisher()

	demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
		if frame.FrameType == avframe.FrameTypeSequenceHeader {
			if frame.MediaType.IsVideo() {
				pub.info.VideoCodec = frame.Codec
				pub.info.VideoSequenceHeader = frame.Payload
			} else if frame.MediaType.IsAudio() {
				pub.info.AudioCodec = frame.Codec
				pub.info.AudioSequenceHeader = frame.Payload
			}
		}
		stream.WriteFrame(frame)
	})

	buf := make([]byte, 1500)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			demuxer.Flush()
			// If context was cancelled, this is a normal shutdown
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("srt read: %w", err)
		}
		if n > 0 {
			demuxer.Feed(buf[:n])
		}
	}
}

func (t *SRTTransport) Close() error { return nil }

// buildConfig creates a gosrt.Config from the cluster SRT settings.
func (t *SRTTransport) buildConfig() gosrt.Config {
	cfg := gosrt.DefaultConfig()
	if t.cfg.Latency > 0 {
		cfg.ReceiverLatency = t.cfg.Latency
		cfg.PeerLatency = t.cfg.Latency
	}
	if t.cfg.Passphrase != "" {
		cfg.Passphrase = t.cfg.Passphrase
		if t.cfg.PBKeyLen > 0 {
			cfg.PBKeylen = t.cfg.PBKeyLen
		}
	}
	return cfg
}

// parseSRTURL parses an SRT URL into host:port and stream path.
// Format: srt://host:port/path or srt://host:port?streamid=path
func parseSRTURL(rawURL string) (addr, streamID string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid SRT URL: %w", err)
	}
	if u.Scheme != "srt" {
		return "", "", fmt.Errorf("unsupported scheme %q, want srt", u.Scheme)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("missing host in SRT URL")
	}

	addr = u.Host
	if u.Port() == "" {
		addr += ":6000"
	}

	// Check for streamid query param first, then use path
	if sid := u.Query().Get("streamid"); sid != "" {
		streamID = sid
	} else if u.Path != "" && u.Path != "/" {
		streamID = u.Path
	} else {
		return "", "", fmt.Errorf("missing stream path in SRT URL")
	}

	return addr, streamID, nil
}

// defaultClusterSRTConfig returns a config with defaults for testing.
func defaultClusterSRTConfig() config.ClusterSRTConfig {
	return config.ClusterSRTConfig{
		Latency:  120 * time.Millisecond,
		PBKeyLen: 16,
	}
}
