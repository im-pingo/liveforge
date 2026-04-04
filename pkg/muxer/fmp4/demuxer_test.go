package fmp4

import (
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
