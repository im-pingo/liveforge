package fmp4

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestDemuxerRoundTrip(t *testing.T) {
	// Create test frames
	videoSeqHeader := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH264,
		FrameType: avframe.FrameTypeSequenceHeader,
		DTS:       0,
		PTS:       0,
		Payload:   []byte{0x01, 0x64, 0x00, 0x28}, // minimal avcC-like data
	}
	audioSeqHeader := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		FrameType: avframe.FrameTypeSequenceHeader,
		DTS:       0,
		PTS:       0,
		Payload:   []byte{0x12, 0x10}, // AAC-LC 44100Hz stereo
	}

	keyframe := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH264,
		FrameType: avframe.FrameTypeKeyframe,
		DTS:       0,
		PTS:       0,
		Payload:   []byte{0x00, 0x00, 0x00, 0x04, 0x65, 0x88, 0x00, 0x01}, // AVCC IDR
	}
	audioFrame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		FrameType: avframe.FrameTypeInterframe,
		DTS:       0,
		PTS:       0,
		Payload:   []byte{0xDE, 0xAD, 0xBE, 0xEF},
	}

	// Mux: create init and media segments
	m := NewMuxer(avframe.CodecH264, avframe.CodecAAC)
	initSeg := m.Init(videoSeqHeader, audioSeqHeader, 640, 360, 44100, 2)
	mediaSeg := m.WriteSegment([]*avframe.AVFrame{keyframe, audioFrame})

	if len(initSeg) == 0 {
		t.Fatal("empty init segment")
	}
	if len(mediaSeg) == 0 {
		t.Fatal("empty media segment")
	}

	// Demux
	d, err := NewDemuxer(initSeg)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}

	frames, err := d.Parse(mediaSeg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Verify: should have at least 1 video + 1 audio frame
	var hasVideo, hasAudio bool
	for _, f := range frames {
		if f.MediaType.IsVideo() {
			hasVideo = true
		}
		if f.MediaType.IsAudio() {
			hasAudio = true
		}
	}
	if !hasVideo || !hasAudio {
		t.Errorf("expected video+audio, got video=%v audio=%v", hasVideo, hasAudio)
	}
}

func TestDemuxerRoundTripPayload(t *testing.T) {
	videoPayload := []byte{0x00, 0x00, 0x00, 0x04, 0x65, 0x88, 0x00, 0x01}
	audioPayload := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	keyframe := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH264,
		FrameType: avframe.FrameTypeKeyframe,
		DTS:       100,
		PTS:       100,
		Payload:   videoPayload,
	}
	audioFrame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		FrameType: avframe.FrameTypeInterframe,
		DTS:       100,
		PTS:       100,
		Payload:   audioPayload,
	}

	m := NewMuxer(avframe.CodecH264, avframe.CodecAAC)
	initSeg := m.Init(nil, &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   []byte{0x12, 0x10},
	}, 640, 360, 44100, 2)
	mediaSeg := m.WriteSegment([]*avframe.AVFrame{keyframe, audioFrame})

	d, err := NewDemuxer(initSeg)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}

	frames, err := d.Parse(mediaSeg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Verify payload integrity
	var videoFound, audioFound bool
	for _, f := range frames {
		if f.MediaType.IsVideo() {
			videoFound = true
			if len(f.Payload) != len(videoPayload) {
				t.Errorf("video payload length = %d, want %d", len(f.Payload), len(videoPayload))
			}
			for i := range f.Payload {
				if f.Payload[i] != videoPayload[i] {
					t.Errorf("video payload[%d] = 0x%02x, want 0x%02x", i, f.Payload[i], videoPayload[i])
					break
				}
			}
		}
		if f.MediaType.IsAudio() {
			audioFound = true
			if len(f.Payload) != len(audioPayload) {
				t.Errorf("audio payload length = %d, want %d", len(f.Payload), len(audioPayload))
			}
			for i := range f.Payload {
				if f.Payload[i] != audioPayload[i] {
					t.Errorf("audio payload[%d] = 0x%02x, want 0x%02x", i, f.Payload[i], audioPayload[i])
					break
				}
			}
		}
	}
	if !videoFound {
		t.Error("no video frame found")
	}
	if !audioFound {
		t.Error("no audio frame found")
	}
}

func TestDemuxerVideoOnly(t *testing.T) {
	keyframe := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH264,
		FrameType: avframe.FrameTypeKeyframe,
		DTS:       0,
		PTS:       0,
		Payload:   []byte{0x00, 0x00, 0x00, 0x04, 0x65, 0x88, 0x00, 0x01},
	}
	interframe := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH264,
		FrameType: avframe.FrameTypeInterframe,
		DTS:       33,
		PTS:       33,
		Payload:   []byte{0x00, 0x00, 0x00, 0x04, 0x41, 0x9A, 0x00, 0x02},
	}

	// Video-only init (audioCodec=0)
	initSeg := BuildInitSegment(avframe.CodecH264, 0, nil, nil, 640, 360, 0, 0)
	mediaSeg := BuildMediaSegment([]*avframe.AVFrame{keyframe, interframe}, 1, 0)

	d, err := NewDemuxer(initSeg)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}

	frames, err := d.Parse(mediaSeg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(frames) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(frames))
	}
	for _, f := range frames {
		if !f.MediaType.IsVideo() {
			t.Errorf("expected video frame, got mediaType=%d", f.MediaType)
		}
	}
	// First frame should be keyframe
	if !frames[0].FrameType.IsKeyframe() {
		t.Errorf("first frame should be keyframe, got frameType=%d", frames[0].FrameType)
	}
	// Second frame should be interframe
	if frames[1].FrameType != avframe.FrameTypeInterframe {
		t.Errorf("second frame should be interframe, got frameType=%d", frames[1].FrameType)
	}
}

func TestDemuxerAudioOnly(t *testing.T) {
	af1 := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		FrameType: avframe.FrameTypeInterframe,
		DTS:       0,
		PTS:       0,
		Payload:   []byte{0xDE, 0xAD},
	}
	af2 := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		FrameType: avframe.FrameTypeInterframe,
		DTS:       23,
		PTS:       23,
		Payload:   []byte{0xBE, 0xEF},
	}

	// Audio-only init (videoCodec=0)
	initSeg := BuildInitSegment(0, avframe.CodecAAC, nil, []byte{0x12, 0x10}, 0, 0, 44100, 2)
	mediaSeg := BuildMediaSegment([]*avframe.AVFrame{af1, af2}, 1, 44100)

	d, err := NewDemuxer(initSeg)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}

	frames, err := d.Parse(mediaSeg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(frames) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(frames))
	}
	for _, f := range frames {
		if !f.MediaType.IsAudio() {
			t.Errorf("expected audio frame, got mediaType=%d", f.MediaType)
		}
	}
}

func TestDemuxerMultiSampleTrun(t *testing.T) {
	// Multiple video frames in a single segment
	var frames []*avframe.AVFrame
	for i := 0; i < 5; i++ {
		ft := avframe.FrameTypeInterframe
		if i == 0 {
			ft = avframe.FrameTypeKeyframe
		}
		frames = append(frames, &avframe.AVFrame{
			MediaType: avframe.MediaTypeVideo,
			Codec:     avframe.CodecH264,
			FrameType: ft,
			DTS:       int64(i * 33),
			PTS:       int64(i * 33),
			Payload:   []byte{0x00, 0x00, 0x00, 0x02, 0x65, byte(i)},
		})
	}

	initSeg := BuildInitSegment(avframe.CodecH264, 0, nil, nil, 640, 360, 0, 0)
	mediaSeg := BuildMediaSegment(frames, 1, 0)

	d, err := NewDemuxer(initSeg)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}

	parsed, err := d.Parse(mediaSeg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(parsed) != 5 {
		t.Fatalf("expected 5 frames, got %d", len(parsed))
	}

	// Verify each payload matches
	for i, f := range parsed {
		expected := []byte{0x00, 0x00, 0x00, 0x02, 0x65, byte(i)}
		if len(f.Payload) != len(expected) {
			t.Errorf("frame[%d] payload length = %d, want %d", i, len(f.Payload), len(expected))
			continue
		}
		for j := range f.Payload {
			if f.Payload[j] != expected[j] {
				t.Errorf("frame[%d] payload[%d] = 0x%02x, want 0x%02x", i, j, f.Payload[j], expected[j])
				break
			}
		}
	}
}

func TestDemuxerInvalidInit(t *testing.T) {
	_, err := NewDemuxer(nil)
	if err == nil {
		t.Error("expected error for nil init segment")
	}

	_, err = NewDemuxer([]byte{0x00, 0x01})
	if err == nil {
		t.Error("expected error for too-short init segment")
	}
}

func TestDemuxerInvalidMedia(t *testing.T) {
	initSeg := BuildInitSegment(avframe.CodecH264, avframe.CodecAAC, nil, []byte{0x12, 0x10}, 640, 360, 44100, 2)
	d, err := NewDemuxer(initSeg)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}

	_, err = d.Parse(nil)
	if err == nil {
		t.Error("expected error for nil media segment")
	}

	_, err = d.Parse([]byte{0x00, 0x01})
	if err == nil {
		t.Error("expected error for too-short media segment")
	}
}

func TestDemuxerCompositionTimeOffset(t *testing.T) {
	// Use the existing muxer to generate a valid init segment so the demuxer
	// can learn track IDs, codecs, and timescales.  Then hand-craft a
	// moof+mdat media segment whose trun includes the 0x000800 flag
	// (sample-composition-time-offset-present) with non-zero CTO values.
	// The demuxer must produce PTS = DTS + CTO (converted to milliseconds).

	initSeg := BuildInitSegment(avframe.CodecH264, 0, nil, nil, 640, 360, 0, 0)

	d, err := NewDemuxer(initSeg)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}

	// --- hand-craft the media segment ---
	// Video timescale is 90000 (see init_segment.go timescaleVideo).
	// We create 3 samples at DTS 0, 3000, 6000 ticks (0, 33, 66 ms).
	// CTO values:  9000, 3000, 0  ticks  (100, 33, 0 ms).
	// Expected PTS: 0+100=100, 33+33=66, 66+0=66 ms (roughly).

	type sample struct {
		duration uint32
		size     uint32
		flags    uint32
		cto      int32
		payload  []byte
	}
	samples := []sample{
		{duration: 3000, size: 6, flags: 0x02000000, cto: 9000, payload: []byte{0x00, 0x00, 0x00, 0x02, 0x65, 0x01}},
		{duration: 3000, size: 6, flags: 0x01010000, cto: 3000, payload: []byte{0x00, 0x00, 0x00, 0x02, 0x41, 0x02}},
		{duration: 3000, size: 6, flags: 0x01010000, cto: 0, payload: []byte{0x00, 0x00, 0x00, 0x02, 0x41, 0x03}},
	}

	// -- trun --
	// flags: data-offset(0x1) | duration(0x100) | size(0x200) | flags(0x400) | CTO(0x800)
	trunFlags := uint32(0x000001 | 0x000100 | 0x000200 | 0x000400 | 0x000800)
	var trunPayload bytes.Buffer
	binary.Write(&trunPayload, binary.BigEndian, uint32(len(samples))) // sample_count
	binary.Write(&trunPayload, binary.BigEndian, uint32(0))           // data_offset placeholder
	for _, s := range samples {
		binary.Write(&trunPayload, binary.BigEndian, s.duration)
		binary.Write(&trunPayload, binary.BigEndian, s.size)
		binary.Write(&trunPayload, binary.BigEndian, s.flags)
		binary.Write(&trunPayload, binary.BigEndian, s.cto)
	}

	// -- tfhd (track 1, default-base-is-moof) --
	tfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(tfhdPayload, videoTrackID)

	// -- tfdt v1 (baseMediaDecodeTime = 0) --
	tfdtPayload := make([]byte, 8)

	// Assemble traf = tfhd + tfdt + trun
	var traf bytes.Buffer
	WriteFullBox(&traf, BoxTfhd, 0, 0x020000, tfhdPayload)
	WriteFullBox(&traf, BoxTfdt, 1, 0, tfdtPayload)
	WriteFullBox(&traf, BoxTrun, 0, trunFlags, trunPayload.Bytes())

	// Assemble moof = mfhd + traf
	var moof bytes.Buffer
	mfhdPayload := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdPayload, 1) // sequence_number
	WriteFullBox(&moof, BoxMfhd, 0, 0, mfhdPayload)
	WriteBox(&moof, BoxTraf, traf.Bytes())

	moofBytes := moof.Bytes()
	moofBoxSize := uint32(8 + len(moofBytes))

	// Build mdat payload
	var mdatPayload bytes.Buffer
	for _, s := range samples {
		mdatPayload.Write(s.payload)
	}

	// Patch data_offset in trun: offset from start of moof box to first sample byte.
	// data_offset = moofBoxSize + mdat_header(8)
	dataOffset := moofBoxSize + 8
	patchTrunDataOffset(moofBytes, dataOffset)

	// Write final segment: moof box + mdat box
	var segment bytes.Buffer
	WriteBox(&segment, BoxMoof, moofBytes)
	WriteBox(&segment, BoxMdat, mdatPayload.Bytes())

	// --- parse ---
	frames, err := d.Parse(segment.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(frames) != len(samples) {
		t.Fatalf("expected %d frames, got %d", len(samples), len(frames))
	}

	// Verify PTS != DTS for samples with non-zero CTO, and PTS == DTS when CTO is 0.
	// timescale = 90000, so CTO ticks -> ms = ticks * 1000 / 90000
	for i, f := range frames {
		expectedDTS := int64(0)
		for j := 0; j < i; j++ {
			expectedDTS += int64(samples[j].duration)
		}
		expectedDTSMs := expectedDTS * 1000 / 90000
		expectedPTSMs := expectedDTSMs + int64(samples[i].cto)*1000/90000

		if f.DTS != expectedDTSMs {
			t.Errorf("frame[%d] DTS = %d, want %d", i, f.DTS, expectedDTSMs)
		}
		if f.PTS != expectedPTSMs {
			t.Errorf("frame[%d] PTS = %d, want %d", i, f.PTS, expectedPTSMs)
		}
		if samples[i].cto != 0 && f.PTS == f.DTS {
			t.Errorf("frame[%d] PTS should differ from DTS when CTO != 0 (PTS=%d, DTS=%d)", i, f.PTS, f.DTS)
		}
		if samples[i].cto == 0 && f.PTS != f.DTS {
			t.Errorf("frame[%d] PTS should equal DTS when CTO == 0 (PTS=%d, DTS=%d)", i, f.PTS, f.DTS)
		}
	}
}

// patchTrunDataOffset scans moof inner bytes for the trun box and writes the
// given data_offset value into its data_offset field.
func patchTrunDataOffset(moofBytes []byte, dataOffset uint32) {
	trunType := []byte{'t', 'r', 'u', 'n'}
	offset := 0
	for offset+8 <= len(moofBytes) {
		boxSize := int(binary.BigEndian.Uint32(moofBytes[offset : offset+4]))
		boxType := moofBytes[offset+4 : offset+8]
		if boxSize < 8 || offset+boxSize > len(moofBytes) {
			break
		}
		if bytes.Equal(boxType, []byte{'t', 'r', 'a', 'f'}) {
			// scan children inside traf
			inner := offset + 8
			end := offset + boxSize
			for inner+8 <= end {
				innerSize := int(binary.BigEndian.Uint32(moofBytes[inner : inner+4]))
				if innerSize < 8 || inner+innerSize > end {
					break
				}
				if bytes.Equal(moofBytes[inner+4:inner+8], trunType) {
					// trun layout: header(8) + version+flags(4) + sample_count(4) + data_offset(4)
					doPos := inner + 8 + 4 + 4
					if doPos+4 <= end {
						binary.BigEndian.PutUint32(moofBytes[doPos:], dataOffset)
					}
					return
				}
				inner += innerSize
			}
		}
		offset += boxSize
	}
}

func TestDemuxerTimestamps(t *testing.T) {
	// Verify that DTS values are recovered correctly (within timescale rounding)
	keyframe := &avframe.AVFrame{
		MediaType: avframe.MediaTypeVideo,
		Codec:     avframe.CodecH264,
		FrameType: avframe.FrameTypeKeyframe,
		DTS:       1000,
		PTS:       1000,
		Payload:   []byte{0x00, 0x00, 0x00, 0x02, 0x65, 0x88},
	}
	audioFrame := &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		FrameType: avframe.FrameTypeInterframe,
		DTS:       1000,
		PTS:       1000,
		Payload:   []byte{0xDE, 0xAD},
	}

	m := NewMuxer(avframe.CodecH264, avframe.CodecAAC)
	initSeg := m.Init(nil, &avframe.AVFrame{
		MediaType: avframe.MediaTypeAudio,
		Codec:     avframe.CodecAAC,
		FrameType: avframe.FrameTypeSequenceHeader,
		Payload:   []byte{0x12, 0x10},
	}, 640, 360, 44100, 2)
	mediaSeg := m.WriteSegment([]*avframe.AVFrame{keyframe, audioFrame})

	d, err := NewDemuxer(initSeg)
	if err != nil {
		t.Fatalf("NewDemuxer: %v", err)
	}

	frames, err := d.Parse(mediaSeg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for _, f := range frames {
		// DTS should be close to 1000ms (may differ slightly due to timescale rounding)
		if f.DTS < 999 || f.DTS > 1001 {
			t.Errorf("frame DTS = %d, want ~1000 (mediaType=%d)", f.DTS, f.MediaType)
		}
	}
}
