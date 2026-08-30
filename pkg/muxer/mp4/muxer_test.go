package mp4

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

type memSeeker struct {
	bytes.Buffer
	pos int64
}

func (m *memSeeker) Write(p []byte) (int, error) {
	// Ensure we write at the current position
	if m.pos < int64(m.Len()) {
		// Overwrite existing data
		data := m.Bytes()
		n := copy(data[m.pos:], p)
		if n < len(p) {
			m.Buffer.Write(p[n:])
		}
		m.pos += int64(len(p))
		return len(p), nil
	}
	// Extend
	if m.pos > int64(m.Len()) {
		padding := make([]byte, m.pos-int64(m.Len()))
		m.Buffer.Write(padding)
	}
	n, err := m.Buffer.Write(p)
	m.pos += int64(n)
	return n, err
}

func (m *memSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.pos = offset
	case io.SeekCurrent:
		m.pos += offset
	case io.SeekEnd:
		m.pos = int64(m.Len()) + offset
	}
	return m.pos, nil
}

func TestMuxerFtypBox(t *testing.T) {
	m := NewMuxer(avframe.CodecH264, avframe.CodecAAC)
	var buf bytes.Buffer
	m.WriteFtyp(&buf)

	data := buf.Bytes()
	if len(data) < 8 {
		t.Fatal("ftyp too short")
	}
	boxType := string(data[4:8])
	if boxType != "ftyp" {
		t.Errorf("expected ftyp box, got %s", boxType)
	}
}

func TestMuxerWriteAndFinalize(t *testing.T) {
	m := NewMuxer(avframe.CodecH264, avframe.CodecAAC)
	w := &memSeeker{}

	if err := m.WriteFtyp(w); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WriteMdatHeader(w); err != nil {
		t.Fatal(err)
	}

	// Write video sequence header
	seqFrame := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64, 0x00, 0x28, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68, 0xEE, 0x3C, 0x80},
	)
	if _, err := m.WriteFrame(w, seqFrame); err != nil {
		t.Fatal(err)
	}

	// Write some video frames
	for i := range 5 {
		ft := avframe.FrameTypeInterframe
		if i == 0 {
			ft = avframe.FrameTypeKeyframe
		}
		dts := int64(i * 33)
		pts := dts
		frame := avframe.NewAVFrame(
			avframe.MediaTypeVideo, avframe.CodecH264, ft,
			pts, dts, []byte{0x00, 0x00, 0x00, byte(i + 1), 0x65, 0x88},
		)
		if _, err := m.WriteFrame(w, frame); err != nil {
			t.Fatal(err)
		}
	}

	if err := m.Finalize(w); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	data := w.Bytes()
	if len(data) < 100 {
		t.Fatalf("output too small: %d bytes", len(data))
	}

	// Verify we have ftyp, mdat, and moov boxes
	foundFtyp := false
	foundMdat := false
	foundMoov := false
	pos := 0
	for pos < len(data)-8 {
		size := binary.BigEndian.Uint32(data[pos : pos+4])
		boxType := string(data[pos+4 : pos+8])

		switch boxType {
		case "ftyp":
			foundFtyp = true
		case "mdat":
			foundMdat = true
		case "moov":
			foundMoov = true
		}

		if size < 8 || int(size) > len(data)-pos {
			break
		}
		pos += int(size)
	}

	if !foundFtyp {
		t.Error("missing ftyp box")
	}
	if !foundMdat {
		t.Error("missing mdat box")
	}
	if !foundMoov {
		t.Error("missing moov box")
	}

	if len(m.videoSamples) != 5 {
		t.Errorf("expected 5 video samples, got %d", len(m.videoSamples))
	}
}

func TestMuxerEmptyFinalize(t *testing.T) {
	m := NewMuxer(avframe.CodecH264, avframe.CodecAAC)
	w := &memSeeker{}
	if err := m.WriteFtyp(w); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WriteMdatHeader(w); err != nil {
		t.Fatal(err)
	}

	if err := m.Finalize(w); err != nil {
		t.Fatalf("Finalize on empty: %v", err)
	}
}

func TestMuxerInterleavedTracksUseIndependentTimelines(t *testing.T) {
	m := NewMuxer(avframe.CodecH264, avframe.CodecAAC)
	w := &memSeeker{}
	if err := m.WriteFtyp(w); err != nil {
		t.Fatal(err)
	}
	if _, err := m.WriteMdatHeader(w); err != nil {
		t.Fatal(err)
	}
	for _, frame := range []*avframe.AVFrame{
		avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader, 0, 0, []byte{0x12, 0x10}),
		avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 0, 0, []byte{0x01}),
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe, 0, 0, []byte{0x00, 0x00, 0x00, 0x01}),
		avframe.NewAVFrame(avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe, 23, 23, []byte{0x02}),
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe, 40, 40, []byte{0x00, 0x00, 0x00, 0x02}),
	} {
		if _, err := m.WriteFrame(w, frame); err != nil {
			t.Fatal(err)
		}
	}

	if got, want := m.audioSamples[0].duration, uint32(23*44100/1000); got != want {
		t.Fatalf("first audio duration = %d, want %d", got, want)
	}
	if got, want := m.videoSamples[0].duration, uint32(40*90000/1000); got != want {
		t.Fatalf("first video duration = %d, want %d", got, want)
	}
}

func TestMuxerSaturatesOutOfRangeSampleTimings(t *testing.T) {
	m := NewMuxer(avframe.CodecH264, 0)
	w := &memSeeker{}
	frames := []*avframe.AVFrame{
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
			0, 1<<40, []byte{0x01}),
		avframe.NewAVFrame(avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
			1<<40, 0, []byte{0x02}),
	}
	for _, frame := range frames {
		if _, err := m.WriteFrame(w, frame); err != nil {
			t.Fatal(err)
		}
	}

	if got := m.videoSamples[0].duration; got != math.MaxUint32 {
		t.Fatalf("large sample duration = %d, want %d", got, uint32(math.MaxUint32))
	}
	if got := m.videoSamples[0].cts; got != math.MaxInt32 {
		t.Fatalf("large positive CTS = %d, want %d", got, int32(math.MaxInt32))
	}
	if got := m.videoSamples[1].cts; got != math.MinInt32 {
		t.Fatalf("large negative CTS = %d, want %d", got, int32(math.MinInt32))
	}
}

func TestMuxerSaturatesVersionZeroMovieDuration(t *testing.T) {
	m := NewMuxer(avframe.CodecH264, 0)
	m.videoSamples = []sampleEntry{{duration: math.MaxUint32}, {duration: 1}}

	mvhd := m.buildMvhd()
	if got := binary.BigEndian.Uint32(mvhd[16:20]); got != math.MaxUint32 {
		t.Fatalf("movie duration = %d, want %d", got, uint32(math.MaxUint32))
	}

	track := m.buildTrak(true)
	if got := binary.BigEndian.Uint32(track[28:32]); got != math.MaxUint32 {
		t.Fatalf("track duration = %d, want %d", got, uint32(math.MaxUint32))
	}
	tkhdSize := int(binary.BigEndian.Uint32(track[0:4]))
	if got := binary.BigEndian.Uint32(track[tkhdSize+32 : tkhdSize+36]); got != math.MaxUint32 {
		t.Fatalf("media duration = %d, want %d", got, uint32(math.MaxUint32))
	}
}

func TestMuxerNormalizesMovieAndTrackDurationsToMovieTimescale(t *testing.T) {
	m := NewMuxer(avframe.CodecH264, avframe.CodecAAC)
	m.audioSampleRate = 48000
	m.videoSamples = []sampleEntry{{duration: 90000}}
	m.audioSamples = []sampleEntry{{duration: 96000}}

	mvhd := m.buildMvhd()
	if got, want := binary.BigEndian.Uint32(mvhd[16:20]), uint32(180000); got != want {
		t.Fatalf("movie duration = %d, want %d", got, want)
	}

	track := m.buildTrak(false)
	if got, want := binary.BigEndian.Uint32(track[28:32]), uint32(180000); got != want {
		t.Fatalf("audio track duration = %d, want %d", got, want)
	}
	tkhdSize := int(binary.BigEndian.Uint32(track[0:4]))
	if got, want := binary.BigEndian.Uint32(track[tkhdSize+32:tkhdSize+36]), uint32(96000); got != want {
		t.Fatalf("audio media duration = %d, want %d", got, want)
	}
}

func TestBuildEsdsUsesExpandableDescriptorLengths(t *testing.T) {
	esds := buildEsds(make([]byte, 128), 44100)
	length, width, ok := decodeDescriptorLength(esds[1:])
	if !ok {
		t.Fatal("ES descriptor length is not valid expandable-size encoding")
	}
	if want := len(esds) - 1 - width; length != want {
		t.Fatalf("ES descriptor length = %d, want %d", length, want)
	}
}

func TestBuildStblUsesSignedCTTSVersionForNegativeCompositionOffsets(t *testing.T) {
	m := NewMuxer(avframe.CodecH264, 0)
	stbl := m.buildStbl(true, []sampleEntry{
		{cts: 900},
		{cts: -450},
	})

	version, payload := findFullBox(t, stbl, "ctts")
	if version != 1 {
		t.Fatalf("ctts version = %d, want 1 for signed composition offsets", version)
	}
	if got, want := binary.BigEndian.Uint32(payload[:4]), uint32(2); got != want {
		t.Fatalf("ctts entry count = %d, want %d", got, want)
	}
	var got int32
	if err := binary.Read(bytes.NewReader(payload[16:20]), binary.BigEndian, &got); err != nil {
		t.Fatalf("decode signed ctts offset: %v", err)
	}
	if want := int32(-450); got != want {
		t.Fatalf("second ctts offset = %d, want %d", got, want)
	}
}

func TestBuildStblKeepsUnsignedCTTSVersionForNonNegativeCompositionOffsets(t *testing.T) {
	m := NewMuxer(avframe.CodecH264, 0)
	stbl := m.buildStbl(true, []sampleEntry{
		{cts: 0},
		{cts: 450},
	})

	version, payload := findFullBox(t, stbl, "ctts")
	if version != 0 {
		t.Fatalf("ctts version = %d, want 0 for non-negative composition offsets", version)
	}
	if got, want := binary.BigEndian.Uint32(payload[16:20]), uint32(450); got != want {
		t.Fatalf("second ctts offset = %d, want %d", got, want)
	}
}

func findFullBox(t *testing.T, boxes []byte, wantType string) (byte, []byte) {
	t.Helper()
	for offset := 0; offset+12 <= len(boxes); {
		size := int(binary.BigEndian.Uint32(boxes[offset : offset+4]))
		if size < 12 || offset+size > len(boxes) {
			t.Fatalf("invalid box at offset %d with size %d", offset, size)
		}
		if string(boxes[offset+4:offset+8]) == wantType {
			return boxes[offset+8], boxes[offset+12 : offset+size]
		}
		offset += size
	}
	t.Fatalf("box %q not found", wantType)
	return 0, nil
}

func decodeDescriptorLength(data []byte) (value, width int, ok bool) {
	for index, b := range data {
		if index == 4 {
			return 0, 0, false
		}
		value = value<<7 | int(b&0x7f)
		if b&0x80 == 0 {
			return value, index + 1, true
		}
	}
	return 0, 0, false
}
