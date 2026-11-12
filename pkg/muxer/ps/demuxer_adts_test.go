package ps

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestDemuxAACTruncatedCRCHeaderDoesNotConsumeSequenceHeader(t *testing.T) {
	demuxer := NewDemuxer()
	packHeader := []byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

	// protection_absent=0 requires a 9-byte ADTS header; this frame claims 8.
	malformed := []byte{0xff, 0xf0, 0x50, 0x80, 0x01, 0x00, 0x00, 0x00}
	frames, err := demuxer.Feed(append(bytes.Clone(packHeader), buildPES(0xc0, malformed, 0, 0)...))
	if err != nil {
		t.Fatalf("Feed malformed ADTS: %v", err)
	}
	if len(frames) != 0 {
		t.Fatalf("malformed ADTS emitted %d frames, want none", len(frames))
	}
	if demuxer.AudioCodec() != 0 {
		t.Fatalf("truncated ADTS header selected codec %v, want unknown", demuxer.AudioCodec())
	}

	valid := []byte{0xff, 0xf0, 0x50, 0x80, 0x01, 0x40, 0x00, 0x00, 0x00, 0x55}
	frames, err = demuxer.Feed(append(bytes.Clone(packHeader), buildPES(0xc0, valid, 20, 20)...))
	if err != nil {
		t.Fatalf("Feed valid ADTS: %v", err)
	}
	if len(frames) != 2 || frames[0].FrameType != avframe.FrameTypeSequenceHeader ||
		!bytes.Equal(frames[0].Payload, []byte{0x12, 0x10}) ||
		frames[1].FrameType != avframe.FrameTypeKeyframe ||
		!bytes.Equal(frames[1].Payload, []byte{0x55}) {
		t.Fatalf("valid ADTS did not emit AAC config and media: %+v", frames)
	}
}

func TestDemuxAACShortSyncCandidateDoesNotSelectG711AFallback(t *testing.T) {
	demuxer := NewDemuxer()
	packHeader := []byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	frames, err := demuxer.Feed(append(bytes.Clone(packHeader), buildPES(0xc0, []byte{0xff, 0xf1}, 0, 0)...))
	if err != nil {
		t.Fatalf("Feed short ADTS sync: %v", err)
	}
	if len(frames) != 0 || demuxer.AudioCodec() != 0 {
		t.Fatalf("short ADTS sync did not leave codec unknown: frames=%+v codec=%v", frames, demuxer.AudioCodec())
	}

	valid := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x00, 0x00, 0x55}
	frames, err = demuxer.Feed(append(bytes.Clone(packHeader), buildPES(0xc0, valid, 20, 20)...))
	if err != nil {
		t.Fatalf("Feed valid ADTS: %v", err)
	}
	if len(frames) != 2 || frames[0].FrameType != avframe.FrameTypeSequenceHeader ||
		!bytes.Equal(frames[1].Payload, []byte{0x55}) {
		t.Fatalf("valid ADTS did not initialize after short sync: %+v", frames)
	}
}

func TestDemuxAACShortDeclaredFrameResyncsWithinPES(t *testing.T) {
	demuxer := NewDemuxer()
	packHeader := []byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	// The first synced frame declares six bytes, shorter than any ADTS header.
	malformed := []byte{0xff, 0xf1, 0x50, 0x80, 0x00, 0xc0, 0x00}
	valid := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x00, 0x00, 0x55}
	payload := append(malformed, valid...)
	frames, err := demuxer.Feed(append(bytes.Clone(packHeader), buildPES(0xc0, payload, 0, 0)...))
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(frames) != 2 || frames[0].FrameType != avframe.FrameTypeSequenceHeader ||
		!bytes.Equal(frames[0].Payload, []byte{0x12, 0x10}) ||
		!bytes.Equal(frames[1].Payload, []byte{0x55}) {
		t.Fatalf("valid ADTS after short frame was lost: %+v", frames)
	}
}

func TestDemuxAACTruncatedDeclaredFrameResyncsWithinPES(t *testing.T) {
	demuxer := NewDemuxer()
	packHeader := []byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	// The first synced frame declares 32 bytes but ends before a complete frame.
	malformed := []byte{0xff, 0xf1, 0x50, 0x80, 0x04, 0x00, 0x00}
	valid := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x00, 0x00, 0x55}
	frames, err := demuxer.Feed(append(bytes.Clone(packHeader), buildPES(0xc0, append(malformed, valid...), 0, 0)...))
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(frames) != 2 || frames[0].FrameType != avframe.FrameTypeSequenceHeader ||
		!bytes.Equal(frames[0].Payload, []byte{0x12, 0x10}) ||
		!bytes.Equal(frames[1].Payload, []byte{0x55}) {
		t.Fatalf("valid ADTS after truncated frame was lost: %+v", frames)
	}
}

func TestDemuxAACDetectsCompleteFrameAfterDamagedPayloadPrefix(t *testing.T) {
	demuxer := NewDemuxer()
	packHeader := []byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	// No Program Stream Map is present. The PES starts with damaged bytes, then
	// contains two consecutive complete ADTS frames that identify AAC.
	payload := []byte{0x00, 0x7f, 0xff, 0xf1, 0x50, 0x80, 0x01, 0x00, 0x00, 0x55,
		0xff, 0xf1, 0x50, 0x80, 0x01, 0x00, 0x00, 0x66}
	frames, err := demuxer.Feed(append(bytes.Clone(packHeader), buildPES(0xc0, payload, 0, 0)...))
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if demuxer.AudioCodec() != avframe.CodecAAC {
		t.Fatalf("detected audio codec = %v, want AAC", demuxer.AudioCodec())
	}
	if len(frames) != 3 || frames[0].FrameType != avframe.FrameTypeSequenceHeader ||
		!bytes.Equal(frames[0].Payload, []byte{0x12, 0x10}) ||
		frames[1].FrameType != avframe.FrameTypeKeyframe ||
		!bytes.Equal(frames[1].Payload, []byte{0x55}) ||
		!bytes.Equal(frames[2].Payload, []byte{0x66}) {
		t.Fatalf("AAC after damaged prefix was not emitted: %+v", frames)
	}
}

func TestDemuxPSMLessPCMAWithEmbeddedADTSShapeRemainsPCMA(t *testing.T) {
	demuxer := NewDemuxer()
	packHeader := []byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	first := []byte{0x42, 0xff, 0xf1, 0x50, 0x80, 0x01, 0x00, 0x00, 0x55, 0x77}
	second := []byte{0x12, 0x34, 0x56}
	for i, payload := range [][]byte{first, second} {
		frames, err := demuxer.Feed(append(bytes.Clone(packHeader), buildPES(0xc0, payload, int64(i*20), int64(i*20))...))
		if err != nil {
			t.Fatalf("Feed PCMA packet %d: %v", i, err)
		}
		if demuxer.AudioCodec() != avframe.CodecG711A || len(frames) != 1 ||
			frames[0].Codec != avframe.CodecG711A || !bytes.Equal(frames[0].Payload, payload) {
			t.Fatalf("PCMA packet %d was misclassified or dropped: codec=%v frames=%+v", i, demuxer.AudioCodec(), frames)
		}
	}
}

func TestDemuxAACHeaderOnlyFramesDoNotInitializeDecoder(t *testing.T) {
	tests := []struct {
		name      string
		malformed []byte
	}{
		{"seven-byte header", []byte{0xff, 0xf1, 0x50, 0x80, 0x00, 0xe0, 0x00}},
		{"nine-byte CRC header", []byte{0xff, 0xf0, 0x50, 0x80, 0x01, 0x20, 0x00, 0x00, 0x00}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			demuxer := NewDemuxer()
			packHeader := []byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
			frames, err := demuxer.Feed(append(bytes.Clone(packHeader), buildPES(0xc0, tt.malformed, 0, 0)...))
			if err != nil {
				t.Fatalf("Feed header-only ADTS: %v", err)
			}
			if len(frames) != 0 {
				t.Fatalf("header-only ADTS emitted %d frames", len(frames))
			}
			valid := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x00, 0x00, 0x55}
			frames, err = demuxer.Feed(append(bytes.Clone(packHeader), buildPES(0xc0, valid, 20, 20)...))
			if err != nil {
				t.Fatalf("Feed valid ADTS: %v", err)
			}
			if len(frames) != 2 || frames[0].FrameType != avframe.FrameTypeSequenceHeader ||
				!bytes.Equal(frames[1].Payload, []byte{0x55}) {
				t.Fatalf("valid ADTS did not initialize after header-only frame: %+v", frames)
			}
		})
	}
}

func TestDemuxAACInvalidHeaderFieldsDoNotInitializeDecoder(t *testing.T) {
	for _, tt := range []struct {
		name      string
		malformed []byte
	}{
		{
			name:      "reserved frequency index 13",
			malformed: []byte{0xff, 0xf1, 0x74, 0x80, 0x01, 0x00, 0x00, 0x55},
		},
		{
			name:      "reserved frequency index 14",
			malformed: []byte{0xff, 0xf1, 0x78, 0x80, 0x01, 0x00, 0x00, 0x55},
		},
		{
			name:      "reserved frequency index",
			malformed: []byte{0xff, 0xf1, 0x7c, 0x80, 0x01, 0x00, 0x00, 0x55},
		},
		{
			name:      "channel configuration zero",
			malformed: []byte{0xff, 0xf1, 0x50, 0x00, 0x01, 0x00, 0x00, 0x55},
		},
		{
			name:      "reserved layer",
			malformed: []byte{0xff, 0xf7, 0x50, 0x80, 0x01, 0xe0, 0x00, 0xff, 0xf1, 0x50, 0x80, 0x01, 0x00, 0x00, 0x55},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			demuxer := NewDemuxer()
			packHeader := []byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
			frames, err := demuxer.Feed(append(bytes.Clone(packHeader), buildPES(0xc0, tt.malformed, 0, 0)...))
			if err != nil {
				t.Fatalf("Feed malformed ADTS: %v", err)
			}
			if len(frames) != 0 {
				t.Fatalf("invalid ADTS configuration emitted %d frames, want none", len(frames))
			}

			valid := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x00, 0x00, 0x66}
			frames, err = demuxer.Feed(append(bytes.Clone(packHeader), buildPES(0xc0, valid, 20, 20)...))
			if err != nil {
				t.Fatalf("Feed valid ADTS: %v", err)
			}
			if len(frames) != 2 || frames[0].FrameType != avframe.FrameTypeSequenceHeader ||
				!bytes.Equal(frames[1].Payload, []byte{0x66}) {
				t.Fatalf("valid ADTS did not initialize after invalid configuration: %+v", frames)
			}
		})
	}
}
