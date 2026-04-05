package audiocodec

import (
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

// mockDecoder / mockEncoder implement the interfaces for testing.
type mockDecoder struct{ rate, ch int }

func (m *mockDecoder) SetExtradata([]byte) {}
func (m *mockDecoder) Decode([]byte) (*PCMFrame, error) {
	return &PCMFrame{Samples: []int16{0}, SampleRate: m.rate, Channels: m.ch}, nil
}
func (m *mockDecoder) SampleRate() int { return m.rate }
func (m *mockDecoder) Channels() int   { return m.ch }
func (m *mockDecoder) Close()          {}

type mockEncoder struct{ rate, ch, fs int }

func (m *mockEncoder) Encode(*PCMFrame) ([]byte, error) { return []byte{0}, nil }
func (m *mockEncoder) SampleRate() int                   { return m.rate }
func (m *mockEncoder) Channels() int                     { return m.ch }
func (m *mockEncoder) FrameSize() int                    { return m.fs }
func (m *mockEncoder) Close()                            {}

func TestRegistryNewDecoder(t *testing.T) {
	r := &Registry{
		decoders: make(map[avframe.CodecType]DecoderFactory),
		encoders: make(map[avframe.CodecType]EncoderFactory),
	}
	r.RegisterDecoder(avframe.CodecAAC, func() Decoder {
		return &mockDecoder{rate: 44100, ch: 2}
	})

	dec, err := r.NewDecoder(avframe.CodecAAC)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dec.SampleRate() != 44100 {
		t.Fatalf("expected 44100, got %d", dec.SampleRate())
	}
	dec.Close()

	_, err = r.NewDecoder(avframe.CodecOpus)
	if err == nil {
		t.Fatal("expected error for unregistered codec")
	}
}

func TestRegistryNewEncoder(t *testing.T) {
	r := &Registry{
		decoders: make(map[avframe.CodecType]DecoderFactory),
		encoders: make(map[avframe.CodecType]EncoderFactory),
	}
	r.RegisterEncoder(avframe.CodecOpus, func() Encoder {
		return &mockEncoder{rate: 48000, ch: 2, fs: 960}
	})

	enc, err := r.NewEncoder(avframe.CodecOpus)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enc.FrameSize() != 960 {
		t.Fatalf("expected 960, got %d", enc.FrameSize())
	}
	enc.Close()
}

func TestRegistryCanTranscode(t *testing.T) {
	r := &Registry{
		decoders: make(map[avframe.CodecType]DecoderFactory),
		encoders: make(map[avframe.CodecType]EncoderFactory),
	}
	r.RegisterDecoder(avframe.CodecAAC, func() Decoder { return &mockDecoder{} })
	r.RegisterEncoder(avframe.CodecOpus, func() Encoder { return &mockEncoder{} })

	if !r.CanTranscode(avframe.CodecAAC, avframe.CodecOpus) {
		t.Fatal("expected CanTranscode(AAC, Opus) = true")
	}
	if r.CanTranscode(avframe.CodecOpus, avframe.CodecAAC) {
		t.Fatal("expected CanTranscode(Opus, AAC) = false (no Opus decoder)")
	}
}

func TestRegistrySequenceHeader(t *testing.T) {
	r := &Registry{
		decoders: make(map[avframe.CodecType]DecoderFactory),
		encoders: make(map[avframe.CodecType]EncoderFactory),
		seqHdrs:  make(map[avframe.CodecType]SeqHeaderFactory),
	}
	r.RegisterSequenceHeader(avframe.CodecAAC, func() SequenceHeaderFunc {
		return func() []byte { return []byte{0x12, 0x10} }
	})

	hdr := r.SequenceHeader(avframe.CodecAAC)
	if len(hdr) != 2 {
		t.Fatalf("expected 2 bytes, got %d", len(hdr))
	}
	if r.SequenceHeader(avframe.CodecOpus) != nil {
		t.Fatal("expected nil for Opus sequence header")
	}
}
