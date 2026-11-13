package ps

import (
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestDemuxPSMRejectsLengthsOutsideDeclaredMap(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "program info consumes CRC with trailing bytes",
			data: []byte{
				0, 0, 1, 0xbc, 0, 10, // start code and declared map length
				0xe0, 0xff, 0, 3, // flags and oversized program info length
				0, 0, 0, 0, 0, 0, // rest of declared map
				0, 0, 0, 0, // unrelated trailing bytes
			},
		},
		{
			name: "elementary stream map consumes CRC",
			data: []byte{
				0, 0, 1, 0xbc, 0, 14,
				0xe0, 0xff, 0, 0,
				0, 8, // only one entry fits before the four-byte CRC
				0x1b, 0xe0, 0, 0,
				0x0f, 0xc0, 0, 0, // these bytes occupy the CRC trailer
			},
		},
		{
			name: "elementary stream map leaves undeclared bytes before CRC",
			data: []byte{
				0, 0, 1, 0xbc, 0, 14,
				0xe0, 0xff, 0, 0,
				0, 0, // no elementary stream entries declared
				0x1b, 0xe0, 0, 0, // undeclared bytes before CRC
				0, 0, 0, 0, // CRC
			},
		},
		{
			name: "elementary stream info crosses map boundary",
			data: []byte{
				0, 0, 1, 0xbc, 0, 14,
				0xe0, 0xff, 0, 0, // no program info
				0, 4, // one four-byte elementary stream entry
				0x1b, 0xe0, 0, 5, // H.264 entry falsely claims five info bytes
				0, 0, 0, 0, // CRC bytes are outside the ES map
			},
		},
		{
			name: "earlier valid codec is not committed before a later malformed entry",
			data: []byte{
				0, 0, 1, 0xbc, 0, 18,
				0xe0, 0xff, 0, 0,
				0, 8, // two elementary stream entries
				0x1b, 0xe0, 0, 0, // valid H.264 video
				0x0f, 0xc0, 0, 5, // AAC info exceeds its declared map
				0, 0, 0, 0, // CRC
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDemuxer()
			if _, err := d.Feed(tt.data); err == nil {
				t.Fatal("Feed accepted a malformed program stream map")
			}
			if got := d.VideoCodec(); got != 0 {
				t.Fatalf("VideoCodec = %v after rejected map, want unset", got)
			}
			if got := d.AudioCodec(); got != 0 {
				t.Fatalf("AudioCodec = %v after rejected map, want unset", got)
			}
		})
	}
}

func TestDemuxPSMDetectsCodecWithinDeclaredMap(t *testing.T) {
	// This wire fixture has MPEG-2 CRC 0xf4dcbd45 over the preceding bytes.
	data := []byte{
		0, 0, 1, 0xbc, 0, 14,
		0xe0, 0xff, 0, 0,
		0, 4,
		0x1b, 0xe0, 0, 0, // H.264 video with no descriptors
		0xf4, 0xdc, 0xbd, 0x45,
	}
	d := NewDemuxer()
	if _, err := d.Feed(data); err != nil {
		t.Fatalf("Feed valid map: %v", err)
	}
	if got := d.VideoCodec(); got != avframe.CodecH264 {
		t.Fatalf("VideoCodec = %v, want H.264", got)
	}
}

func TestDemuxPSMRejectsInvalidCRCWithoutCommittingCodecs(t *testing.T) {
	seed := []byte{
		0, 0, 1, 0xbc, 0, 18,
		0xe0, 0xff, 0, 0,
		0, 8,
		0x24, 0xe0, 0, 0, // H.265 video with no descriptors
		0x90, 0xc0, 0, 0, // G.711A audio with no descriptors
		0x5e, 0x5f, 0x92, 0xc2,
	}
	d := NewDemuxer()
	if _, err := d.Feed(seed); err != nil {
		t.Fatalf("Feed seed map: %v", err)
	}

	valid := []byte{
		0, 0, 1, 0xbc, 0, 14,
		0xe0, 0xff, 0, 0,
		0, 4,
		0x1b, 0xe0, 0, 0, // H.264 video with no descriptors
		0xf4, 0xdc, 0xbd, 0x45,
	}
	for _, tt := range []struct {
		name   string
		mutate func([]byte)
	}{
		{
			name: "map payload",
			mutate: func(data []byte) {
				data[12] ^= 0x01
			},
		},
		{
			name: "CRC trailer",
			mutate: func(data []byte) {
				data[len(data)-1] ^= 0x01
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := append([]byte(nil), valid...)
			tt.mutate(data)
			if _, err := d.Feed(data); err == nil {
				t.Fatal("Feed accepted a program stream map with an invalid CRC")
			}
			if got := d.VideoCodec(); got != avframe.CodecH265 {
				t.Fatalf("VideoCodec = %v after rejected map, want H.265", got)
			}
			if got := d.AudioCodec(); got != avframe.CodecG711A {
				t.Fatalf("AudioCodec = %v after rejected map, want G.711A", got)
			}
		})
	}
}
