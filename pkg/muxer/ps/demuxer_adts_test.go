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
