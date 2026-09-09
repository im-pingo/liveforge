package h264

import (
	"fmt"
	"math/bits"
	"strings"
	"testing"
)

type spsFixture struct {
	profile                          byte
	chroma, separate, interlaced     uint
	cycle, widthMinus1, heightMinus1 uint
	crop                             [4]uint
}

func makeSPSFixture(f spsFixture) []byte {
	var stream []byte
	bit := func(v uint) { stream = append(stream, byte(v&1)) }
	ue := func(v uint) {
		v++
		n := bits.Len(v)
		for i := 1; i < n; i++ {
			bit(0)
		}
		for i := n - 1; i >= 0; i-- {
			bit(v >> i)
		}
	}
	ue(0) // seq_parameter_set_id
	if f.profile != 66 {
		ue(f.chroma)
		if f.chroma == 3 {
			bit(f.separate)
		}
		ue(0)
		ue(0)
		bit(0)
		bit(0)
	}
	ue(0) // log2_max_frame_num_minus4
	ue(1) // pic_order_cnt_type
	bit(0)
	ue(0)
	ue(0)
	ue(f.cycle)
	// Large-count fixtures intentionally end before the claimed offsets.
	if f.cycle <= 256 {
		for i := uint(0); i < f.cycle; i++ {
			ue(0)
		}
		ue(0)
		bit(0)
		ue(f.widthMinus1)
		ue(f.heightMinus1)
		bit(1 - f.interlaced)
		if f.interlaced != 0 {
			bit(0)
		}
		bit(1)
		bit(1)
		for _, v := range f.crop {
			ue(v)
		}
		bit(0)
		bit(1) // no VUI, RBSP stop bit
	}
	raw := []byte{0x67, f.profile, 0, 62}
	for i := 0; i < len(stream); i += 8 {
		var v byte
		for j := 0; j < 8; j++ {
			v <<= 1
			if i+j < len(stream) {
				v |= stream[i+j]
			}
		}
		if len(raw) >= 2 && raw[len(raw)-1] == 0 && raw[len(raw)-2] == 0 && v <= 3 {
			raw = append(raw, 3)
		}
		raw = append(raw, v)
	}
	return raw
}

func TestParseSPSRejectsInvalidCycleAndDimensions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture spsFixture
	}{
		{"cycle exceeds spec", spsFixture{profile: 66, cycle: 256}},
		{"width exceeds maximum level", spsFixture{profile: 66, widthMinus1: 1055}},
		{"height exceeds maximum level", spsFixture{profile: 66, heightMinus1: 1055}},
		{"frame exceeds maximum level", spsFixture{profile: 66, widthMinus1: 511, heightMinus1: 511}},
		{"crop removes width", spsFixture{profile: 66, crop: [4]uint{0, 8, 0, 0}}},
		{"crop exceeds height", spsFixture{profile: 66, crop: [4]uint{0, 0, 9, 0}}},
		{"invalid chroma format", spsFixture{profile: 100, chroma: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if info, err := ParseSPS(makeSPSFixture(tc.fixture)); err == nil || info != nil {
				t.Fatalf("invalid SPS accepted: info=%+v error=%v", info, err)
			}
		})
	}
}

func TestParseSPSValidProfilesAndCropping(t *testing.T) {
	for _, tc := range []struct {
		name          string
		fixture       spsFixture
		width, height int
	}{
		{"max cycle", spsFixture{profile: 66, cycle: 255}, 16, 16},
		{"8K", spsFixture{profile: 100, chroma: 1, widthMinus1: 479, heightMinus1: 269}, 7680, 4320},
		{"maximum frame size", spsFixture{profile: 100, chroma: 1, widthMinus1: 511, heightMinus1: 271}, 8192, 4352},
		{"monochrome", spsFixture{profile: 100, chroma: 0, crop: [4]uint{1, 2, 3, 4}}, 13, 9},
		{"420", spsFixture{profile: 100, chroma: 1, crop: [4]uint{1, 2, 1, 2}}, 10, 10},
		{"422", spsFixture{profile: 122, chroma: 2, crop: [4]uint{1, 2, 3, 4}}, 10, 9},
		{"444", spsFixture{profile: 244, chroma: 3, crop: [4]uint{1, 2, 3, 4}}, 13, 9},
		{"separate colour planes", spsFixture{profile: 244, chroma: 3, separate: 1, crop: [4]uint{1, 2, 3, 4}}, 13, 9},
		{"interlaced 422", spsFixture{profile: 122, chroma: 2, interlaced: 1, crop: [4]uint{1, 2, 3, 4}}, 10, 18},
		{"profile 135", spsFixture{profile: 135, chroma: 1}, 16, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := ParseSPS(makeSPSFixture(tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			if info.Width != tc.width || info.Height != tc.height || info.Profile != int(tc.fixture.profile) {
				t.Fatalf("SPS = %+v, want %dx%d profile %d", info, tc.width, tc.height, tc.fixture.profile)
			}
		})
	}
}

func TestParseSPSTruncated(t *testing.T) {
	sps := makeSPSFixture(spsFixture{profile: 100, chroma: 1, cycle: 255, widthMinus1: 119, heightMinus1: 67, crop: [4]uint{0, 0, 0, 4}})
	for n := 0; n < len(sps)-1; n++ {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			if info, err := ParseSPS(sps[:n]); err == nil || info != nil {
				t.Fatalf("accepted truncated SPS: %+v", info)
			}
		})
	}
}

func TestParseSPSLargeCycleFailsAtCount(t *testing.T) {
	for _, count := range []uint{100000000, 4294967294} {
		_, err := ParseSPS(makeSPSFixture(spsFixture{profile: 66, cycle: count}))
		if err == nil || !strings.Contains(err.Error(), "num_ref_frames_in_pic_order_cnt_cycle") {
			t.Fatalf("count %d did not fail at its bound: %v", count, err)
		}
	}
}

func FuzzParseSPS(f *testing.F) {
	f.Add(makeSPSFixture(spsFixture{profile: 66, cycle: 256}))
	f.Add(makeSPSFixture(spsFixture{profile: 100, chroma: 1, widthMinus1: 119, heightMinus1: 67}))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		info, err := ParseSPS(data)
		if err == nil && (info == nil || info.Width <= 0 || info.Height <= 0 || info.Width > 16880 || info.Height > 16880) {
			t.Fatalf("invalid successful dimensions: %+v", info)
		}
	})
}
