package fmp4

import (
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

func dimensionTestAVCC(sps []byte) []byte {
	if len(sps) > 65535 {
		panic("SPS fixture exceeds avcC length field")
	}
	avcc := make([]byte, 8, 8+len(sps))
	copy(avcc, []byte{1, 66, 0, 30, 0xff, 0xe1, 0, 0})
	binary.BigEndian.PutUint16(avcc[6:], uint16(len(sps))) // #nosec G115 -- The fixture length is checked against 65535 above.
	return append(avcc, sps...)
}

func dimensionTestSPS(body string) []byte {
	body = strings.ReplaceAll(body, " ", "")
	raw := []byte{0x67, 66, 0, 30}
	for len(body)%8 != 0 {
		body += "0"
	}
	for i := 0; i < len(body); i += 8 {
		var v byte
		for j := i; j < i+8; j++ {
			v = v<<1 | (body[j] - '0')
		}
		raw = append(raw, v)
	}
	return raw
}

func TestParseAVCCDimensionsChromaCropping(t *testing.T) {
	for _, tc := range []struct {
		name          string
		profile       byte
		chromaBits    string
		width, height int
	}{
		{"monochrome", 100, "1", 13, 9},
		{"422", 122, "011", 10, 9},
		{"444", 244, "00100 0", 13, 9},
		{"separate colour planes", 244, "00100 1", 13, 9},
		{"profile 135", 135, "011", 10, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 16x16 coded frame with crop offsets left=1,right=2,top=3,bottom=4.
			sps := dimensionTestSPS("1 " + tc.chromaBits + " 1 1 0 0 1 011 1 0 1 1 1 1 1 010 011 00100 00101 0 1")
			sps[1] = tc.profile
			if w, h := ParseAVCCDimensions(dimensionTestAVCC(sps)); w != tc.width || h != tc.height {
				t.Fatalf("got %dx%d, want %dx%d", w, h, tc.width, tc.height)
			}
		})
	}
}

func TestParseAVCCDimensionsRejectsOversizedCycle(t *testing.T) {
	// POC type 1 with 256 encoded offsets, followed by a complete 16x16 SPS.
	sps := dimensionTestSPS("11010011" + "00000000100000001" + strings.Repeat("1", 256) + "101111001")
	if w, h := ParseAVCCDimensions(dimensionTestAVCC(sps)); w != 0 || h != 0 {
		t.Fatalf("oversized POC cycle accepted as %dx%d", w, h)
	}
}

func TestParseAVCCDimensionsValidAndTruncated(t *testing.T) {
	sps, err := hex.DecodeString("67640028acd940780227e5c044000003000400000300f03c60c658")
	if err != nil {
		t.Fatal(err)
	}
	avcc := dimensionTestAVCC(sps)
	if w, h := ParseAVCCDimensions(avcc); w != 1920 || h != 1080 {
		t.Fatalf("got %dx%d", w, h)
	}
	for n := 0; n < len(avcc); n++ {
		if w, h := ParseAVCCDimensions(avcc[:n]); w != 0 || h != 0 {
			t.Fatalf("truncated avcC length %d: %dx%d", n, w, h)
		}
	}
	sps[0] = 0x68
	if w, h := ParseAVCCDimensions(dimensionTestAVCC(sps)); w != 0 || h != 0 {
		t.Fatalf("PPS accepted as SPS: %dx%d", w, h)
	}
}

func FuzzParseAVCCDimensions(f *testing.F) {
	f.Add(dimensionTestAVCC(dimensionTestSPS("11010011" + "00000000100000001" + strings.Repeat("1", 256) + "101111001")))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		w, h := ParseAVCCDimensions(data)
		if w < 0 || h < 0 || w > 16880 || h > 16880 || (w == 0) != (h == 0) {
			t.Fatalf("invalid dimensions %dx%d", w, h)
		}
	})
}
