package ps

import (
	"bytes"
	"testing"
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
