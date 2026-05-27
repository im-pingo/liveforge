package mp4

import (
	"bytes"
	"encoding/binary"
	"io"
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

	m.WriteFtyp(w)
	m.WriteMdatHeader(w)

	// Write video sequence header
	seqFrame := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64, 0x00, 0x28, 0xFF, 0xE1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68, 0xEE, 0x3C, 0x80},
	)
	m.WriteFrame(w, seqFrame, -1)

	// Write some video frames
	var prevDTS int64 = -1
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
		m.WriteFrame(w, frame, prevDTS)
		prevDTS = dts
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
	m.WriteFtyp(w)
	m.WriteMdatHeader(w)

	if err := m.Finalize(w); err != nil {
		t.Fatalf("Finalize on empty: %v", err)
	}
}
