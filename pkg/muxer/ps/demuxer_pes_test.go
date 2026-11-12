package ps

import (
	"bytes"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/rtp"
)

func TestDemuxPESRejectsIncompleteDeclaredFields(t *testing.T) {
	tests := []struct {
		name string
		pes  []byte
	}{
		{
			name: "declared packet exceeds received bytes",
			pes:  []byte{0, 0, 1, 0xc0, 0, 8, 0x80, 0, 0, 0x11, 0x22},
		},
		{
			name: "optional header exceeds declared packet",
			pes:  []byte{0, 0, 1, 0xc0, 0, 4, 0x80, 0, 5, 1, 2, 3, 4, 5, 0x11},
		},
		{
			name: "PTS flag has no declared timestamp bytes",
			pes:  []byte{0, 0, 1, 0xc0, 0, 9, 0x80, 0x80, 0, 0x21, 0, 1, 0, 1, 0x11},
		},
		{
			name: "PTS and DTS flag has only one timestamp",
			pes:  []byte{0, 0, 1, 0xc0, 0, 9, 0x80, 0xc0, 5, 0x31, 0, 1, 0, 1, 0x11},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pack := append([]byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, tt.pes...)
			frames, err := NewDemuxer().Feed(pack)
			if err == nil {
				t.Fatalf("Feed accepted malformed PES and returned %d frames", len(frames))
			}
			if len(frames) != 0 {
				t.Fatalf("Feed returned %d frames from malformed PES", len(frames))
			}
		})
	}
}

func TestDemuxPESRejectsInvalidTimestampEncoding(t *testing.T) {
	tests := []struct {
		name string
		pes  []byte
	}{
		{
			name: "first PTS marker bit is clear",
			pes: func() []byte {
				pes := buildPES(0xc0, []byte{0x11}, 1000, 1000)
				pes[9] &^= 0x01
				return pes
			}(),
		},
		{
			name: "second PTS marker bit is clear",
			pes: func() []byte {
				pes := buildPES(0xc0, []byte{0x11}, 1000, 1000)
				pes[11] &^= 0x01
				return pes
			}(),
		},
		{
			name: "third PTS marker bit is clear",
			pes: func() []byte {
				pes := buildPES(0xc0, []byte{0x11}, 1000, 1000)
				pes[13] &^= 0x01
				return pes
			}(),
		},
		{
			name: "PTS only prefix is wrong",
			pes: func() []byte {
				pes := buildPES(0xc0, []byte{0x11}, 1000, 1000)
				pes[9] = (pes[9] & 0x0f) | 0x30
				return pes
			}(),
		},
		{
			name: "DTS prefix is wrong",
			pes: func() []byte {
				pes := buildPES(0xc0, []byte{0x11}, 1000, 1100)
				pes[14] = (pes[14] & 0x0f) | 0x20
				return pes
			}(),
		},
		{
			name: "DTS marker bit is clear",
			pes: func() []byte {
				pes := buildPES(0xc0, []byte{0x11}, 1000, 1100)
				pes[18] &^= 0x01
				return pes
			}(),
		},
		{
			name: "PTS DTS flags use reserved value",
			pes: func() []byte {
				pes := buildPES(0xc0, []byte{0x11}, 1000, 1000)
				pes[7] = (pes[7] & 0x3f) | 0x40
				return pes
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pack := append([]byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, tt.pes...)
			frames, err := NewDemuxer().Feed(pack)
			if err == nil {
				t.Fatalf("Feed accepted PES with malformed timestamp encoding and returned %d frames", len(frames))
			}
			if len(frames) != 0 {
				t.Fatalf("Feed returned %d frames from PES with malformed timestamp encoding", len(frames))
			}
		})
	}
}

func TestDemuxPESPreservesDistinctTimestamps(t *testing.T) {
	pack := append([]byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		buildPES(0xc0, []byte{0x11}, 1000, 1100)...)
	frames, err := NewDemuxer().Feed(pack)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(frames) != 1 || frames[0].DTS != 1000 || frames[0].PTS != 1100 || !bytes.Equal(frames[0].Payload, []byte{0x11}) {
		t.Fatalf("Feed returned wrong PES media: %+v", frames)
	}
}

func TestDemuxPESZeroLengthConsumesPackRemainder(t *testing.T) {
	pack := append([]byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		[]byte{0, 0, 1, 0xc0, 0, 0, 0x80, 0, 0, 0x22, 0x33}...)
	frames, err := NewDemuxer().Feed(pack)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(frames) != 1 || !bytes.Equal(frames[0].Payload, []byte{0x22, 0x33}) {
		t.Fatalf("Feed returned wrong unbounded PES media: %+v", frames)
	}
}

func TestDemuxPESZeroLengthVideoStopsAtNextPacket(t *testing.T) {
	video := []byte{0, 0, 0, 1, 0x41, 0x88, 0, 0, 1, 0x41, 0x99}
	videoPES := buildPES(0xe0, video, 1000, 1000)
	videoPES[4], videoPES[5] = 0, 0
	pack := append([]byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, videoPES...)
	pack = append(pack, buildPES(0xc0, []byte{0x22, 0x33}, 1020, 1020)...)

	frames, err := NewDemuxer().Feed(pack)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("Feed returned %d frames, want video and audio", len(frames))
	}
	if frames[0].MediaType != avframe.MediaTypeVideo || !bytes.Equal(frames[0].Payload, rtp.AnnexBToAVCC(video)) {
		t.Fatalf("Feed returned wrong video media: %+v", frames[0])
	}
	if frames[1].MediaType != avframe.MediaTypeAudio || frames[1].DTS != 1020 || !bytes.Equal(frames[1].Payload, []byte{0x22, 0x33}) {
		t.Fatalf("Feed returned wrong audio media: %+v", frames[1])
	}
}

func TestDemuxPESZeroLengthVideoStopsAtProgramEnd(t *testing.T) {
	video := []byte{0, 0, 1, 0x41, 0x88}
	videoPES := buildPES(0xe0, video, 1000, 1000)
	videoPES[4], videoPES[5] = 0, 0
	pack := append([]byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, videoPES...)
	pack = append(pack, 0, 0, 1, 0xb9)

	frames, err := NewDemuxer().Feed(pack)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(frames) != 1 || !bytes.Equal(frames[0].Payload, rtp.AnnexBToAVCC(video)) {
		t.Fatalf("Feed included program end in video media: %+v", frames)
	}
}

func TestDemuxPESKeepsEarlierFrameOnLaterError(t *testing.T) {
	pack := append([]byte{0, 0, 1, 0xba, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		buildPES(0xc0, []byte{0x31}, 0, 0)...)
	pack = append(pack, []byte{0, 0, 1, 0xc0, 0, 8, 0x80, 0, 0, 0x41, 0x42}...)
	frames, err := NewDemuxer().Feed(pack)
	if err == nil {
		t.Fatal("Feed accepted truncated second PES")
	}
	if len(frames) != 1 || !bytes.Equal(frames[0].Payload, []byte{0x31}) {
		t.Fatalf("Feed lost or mixed the earlier PES media: %+v", frames)
	}
}
