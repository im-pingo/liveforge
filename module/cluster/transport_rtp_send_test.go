package cluster

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	pkgrtp "github.com/im-pingo/liveforge/pkg/rtp"
	pionrtp "github.com/pion/rtp/v2"
)

type rtpFrameWriterFunc func([]byte) (int, error)

func (f rtpFrameWriterFunc) Write(payload []byte) (int, error) {
	return f(payload)
}

type failingRTPPacketizer struct {
	packets []*pionrtp.Packet
	err     error
}

func (p failingRTPPacketizer) Packetize(*avframe.AVFrame, int) ([]*pionrtp.Packet, error) {
	return p.packets, p.err
}

func rtpTestFrame() *avframe.AVFrame {
	return &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH264,
		Payload:   []byte{0x65},
	}
}

func TestSendRTPFramePropagatesPacketizeError(t *testing.T) {
	want := errors.New("packetize failed")
	frame := rtpTestFrame()

	err := sendRTPFrame(
		context.Background(),
		rtpFrameWriterFunc(func(payload []byte) (int, error) { return len(payload), nil }),
		frame,
		failingRTPPacketizer{err: want},
		pkgrtp.NewSession(96, 90000),
		nil,
	)
	if !errors.Is(err, want) {
		t.Fatalf("sendRTPFrame error = %v, want %v", err, want)
	}
}

func TestSendRTPFramePropagatesMarshalError(t *testing.T) {
	frame := rtpTestFrame()
	packet := &pionrtp.Packet{
		Header:  pionrtp.Header{Extension: true},
		Payload: []byte{0x01},
	}
	packet.ExtensionProfile = 0
	if err := packet.SetExtension(0, []byte{0x01}); err != nil {
		t.Fatalf("SetExtension: %v", err)
	}

	err := sendRTPFrame(
		context.Background(),
		rtpFrameWriterFunc(func(payload []byte) (int, error) { return len(payload), nil }),
		frame,
		failingRTPPacketizer{packets: []*pionrtp.Packet{packet}},
		pkgrtp.NewSession(96, 90000),
		nil,
	)
	if err == nil {
		t.Fatal("sendRTPFrame returned nil for marshal failure")
	}
}

func TestSendRTPFrameRejectsEmptyPacketOutput(t *testing.T) {
	writes := 0
	err := sendRTPFrame(
		context.Background(),
		rtpFrameWriterFunc(func(payload []byte) (int, error) {
			writes++
			return len(payload), nil
		}),
		rtpTestFrame(),
		failingRTPPacketizer{},
		pkgrtp.NewSession(96, 90000),
		nil,
	)
	if err == nil {
		t.Fatal("sendRTPFrame returned nil for empty packet output")
	}
	if writes != 0 {
		t.Fatalf("writer called %d times for empty packet output, want 0", writes)
	}
}

func TestSendRTPFramePropagatesWriteError(t *testing.T) {
	want := errors.New("write failed")
	err := sendRTPFrame(
		context.Background(),
		rtpFrameWriterFunc(func([]byte) (int, error) { return 0, want }),
		rtpTestFrame(),
		failingRTPPacketizer{packets: []*pionrtp.Packet{{Payload: []byte{0x01}}}},
		pkgrtp.NewSession(96, 90000),
		nil,
	)
	if !errors.Is(err, want) {
		t.Fatalf("sendRTPFrame error = %v, want %v", err, want)
	}
}

func TestSendRTPFrameRejectsShortWrite(t *testing.T) {
	err := sendRTPFrame(
		context.Background(),
		rtpFrameWriterFunc(func(payload []byte) (int, error) { return len(payload) - 1, nil }),
		rtpTestFrame(),
		failingRTPPacketizer{packets: []*pionrtp.Packet{{Payload: []byte{0x01}}}},
		pkgrtp.NewSession(96, 90000),
		nil,
	)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("sendRTPFrame error = %v, want %v", err, io.ErrShortWrite)
	}
}
