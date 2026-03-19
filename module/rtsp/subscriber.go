package rtsp

import (
	"io"
	"sync"

	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
)

// RTSPSubscriber implements RTSP playback via TCP interleaved transport.
type RTSPSubscriber struct {
	id      string
	options core.SubscribeOptions
	writer  io.Writer // TCP conn for interleaved mode

	videoPacketizer pkgrtp.Packetizer
	audioPacketizer pkgrtp.Packetizer
	videoSession    *pkgrtp.Session
	audioSession    *pkgrtp.Session
	videoChannel    uint8 // TCP interleaved channel for video RTP
	audioChannel    uint8 // TCP interleaved channel for audio RTP

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

var _ core.Subscriber = (*RTSPSubscriber)(nil)

// NewRTSPSubscriber creates a new RTSP subscriber for TCP interleaved mode.
func NewRTSPSubscriber(id string, info *avframe.MediaInfo, writer io.Writer, videoChannel, audioChannel uint8) (*RTSPSubscriber, error) {
	sub := &RTSPSubscriber{
		id:           id,
		options:      core.DefaultSubscribeOptions(),
		writer:       writer,
		videoChannel: videoChannel,
		audioChannel: audioChannel,
		done:         make(chan struct{}),
	}

	if info.VideoCodec != 0 {
		p, err := pkgrtp.NewPacketizer(info.VideoCodec)
		if err != nil {
			return nil, err
		}
		sub.videoPacketizer = p
		pt := codecDefaultPT(info.VideoCodec)
		sub.videoSession = pkgrtp.NewSession(pt, 90000) // video always 90kHz
	}

	if info.AudioCodec != 0 {
		p, err := pkgrtp.NewPacketizer(info.AudioCodec)
		if err != nil {
			return nil, err
		}
		sub.audioPacketizer = p
		pt := codecDefaultPT(info.AudioCodec)
		clockRate := audioClockRate(info)
		sub.audioSession = pkgrtp.NewSession(pt, clockRate)
	}

	return sub, nil
}

func (s *RTSPSubscriber) ID() string                     { return s.id }
func (s *RTSPSubscriber) Options() core.SubscribeOptions { return s.options }

func (s *RTSPSubscriber) OnData(data []byte) error {
	// Not used for RTSP — RTSP uses SendFrame directly.
	return nil
}

func (s *RTSPSubscriber) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	return nil
}

// SendFrame packetizes an AVFrame and sends it via TCP interleaved transport.
func (s *RTSPSubscriber) SendFrame(frame *avframe.AVFrame) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	var packetizer pkgrtp.Packetizer
	var session *pkgrtp.Session
	var channel uint8

	if frame.MediaType.IsVideo() && s.videoPacketizer != nil {
		packetizer = s.videoPacketizer
		session = s.videoSession
		channel = s.videoChannel
	} else if frame.MediaType.IsAudio() && s.audioPacketizer != nil {
		packetizer = s.audioPacketizer
		session = s.audioSession
		channel = s.audioChannel
	} else {
		return nil
	}

	pkts, err := packetizer.Packetize(frame, pkgrtp.DefaultMTU)
	if err != nil {
		return err
	}

	wrapped := session.WrapPackets(pkts, frame.DTS)

	for _, pkt := range wrapped {
		data, err := pkt.Marshal()
		if err != nil {
			return err
		}
		if err := WriteInterleaved(s.writer, channel, data); err != nil {
			return err
		}
	}

	return nil
}

// Done returns a channel that is closed when the subscriber is closed.
func (s *RTSPSubscriber) Done() <-chan struct{} {
	return s.done
}

// audioClockRate returns the RTP clock rate for the audio codec.
func audioClockRate(info *avframe.MediaInfo) uint32 {
	switch info.AudioCodec {
	case avframe.CodecOpus:
		return 48000
	case avframe.CodecG711U, avframe.CodecG711A, avframe.CodecG722, avframe.CodecG729, avframe.CodecSpeex:
		return 8000
	case avframe.CodecMP3:
		return 90000
	case avframe.CodecAAC:
		if info.SampleRate > 0 {
			return uint32(info.SampleRate)
		}
		return 44100
	default:
		return 44100
	}
}
