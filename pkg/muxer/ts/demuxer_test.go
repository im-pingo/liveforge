package ts

import (
	"encoding/binary"
	"testing"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/aac"
)

func TestCodecFromStreamType(t *testing.T) {
	tests := []struct {
		st    byte
		codec avframe.CodecType
	}{
		{0x1B, avframe.CodecH264},
		{0x24, avframe.CodecH265},
		{0x0F, avframe.CodecAAC},
		{0x03, avframe.CodecMP3},
		{0x04, avframe.CodecMP3},
		{0xFF, 0},
	}
	for _, tt := range tests {
		got := CodecFromStreamType(tt.st)
		if got != tt.codec {
			t.Errorf("CodecFromStreamType(0x%02X) = %v, want %v", tt.st, got, tt.codec)
		}
	}
}

func TestReadTimestamp(t *testing.T) {
	// Encode a known timestamp using writePTS and verify ReadTimestamp recovers it
	tests := []int64{0, 100, 1000, 33333, 90000}
	for _, ms := range tests {
		buf := make([]byte, 5)
		writePTS(buf, ms, false)
		got := ReadTimestamp(buf)
		if got != ms {
			t.Errorf("ReadTimestamp(writePTS(%d)) = %d", ms, got)
		}
	}
}

func TestReadTimestampWithDTS(t *testing.T) {
	// Test DTS encoding/decoding
	tests := []int64{0, 500, 12345}
	for _, ms := range tests {
		buf := make([]byte, 5)
		writeDTS(buf, ms)
		got := ReadTimestamp(buf)
		if got != ms {
			t.Errorf("ReadTimestamp(writeDTS(%d)) = %d", ms, got)
		}
	}
}

func TestDemuxerParsePAT(t *testing.T) {
	pat := BuildPAT(0)
	dmx := NewDemuxer(func(f *avframe.AVFrame) {})
	dmx.Feed(pat)
	if dmx.pmtPID != PIDPmt {
		t.Errorf("PMT PID = 0x%04X, want 0x%04X", dmx.pmtPID, PIDPmt)
	}
}

func TestDemuxerParsePMT(t *testing.T) {
	pat := BuildPAT(0)
	pmt := BuildPMT(0x1B, 0x0F, 0)

	dmx := NewDemuxer(func(f *avframe.AVFrame) {})
	dmx.Feed(append(pat, pmt...))

	if dmx.videoCodec != avframe.CodecH264 {
		t.Errorf("video codec = %v, want H264", dmx.videoCodec)
	}
	if dmx.audioCodec != avframe.CodecAAC {
		t.Errorf("audio codec = %v, want AAC", dmx.audioCodec)
	}
	if dmx.videoPID != PIDVideo {
		t.Errorf("video PID = 0x%04X, want 0x%04X", dmx.videoPID, PIDVideo)
	}
	if dmx.audioPID != PIDAudio {
		t.Errorf("audio PID = 0x%04X, want 0x%04X", dmx.audioPID, PIDAudio)
	}
}

func TestDemuxerRoundTripH264AAC(t *testing.T) {
	// Build source frames
	sps := []byte{0x67, 0x64, 0x00, 0x28, 0xAC}
	pps := []byte{0x68, 0xEE, 0x38}
	avcRecord := buildTestAVCRecord(sps, pps)
	asc := []byte{0x12, 0x10} // AAC-LC, 44100Hz, stereo

	muxer := NewMuxer(avframe.CodecH264, avframe.CodecAAC, avcRecord, asc)

	// Create test frames
	videoSeqHeader := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264,
		avframe.FrameTypeSequenceHeader, 0, 0, avcRecord,
	)
	audioSeqHeader := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC,
		avframe.FrameTypeSequenceHeader, 0, 0, asc,
	)

	// Build AVCC keyframe payload: SPS + PPS + IDR
	idrNAL := make([]byte, 20)
	idrNAL[0] = 0x65 // IDR
	for i := 1; i < len(idrNAL); i++ {
		idrNAL[i] = byte(i)
	}
	var avccPayload []byte
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(idrNAL)))
	avccPayload = append(avccPayload, lenBuf[:]...)
	avccPayload = append(avccPayload, idrNAL...)

	videoKeyframe := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264,
		avframe.FrameTypeKeyframe, 0, 0, avccPayload,
	)

	// Build a P-frame
	pNAL := make([]byte, 10)
	pNAL[0] = 0x41 // Non-IDR
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(pNAL)))
	var avccP []byte
	avccP = append(avccP, lenBuf[:]...)
	avccP = append(avccP, pNAL...)

	videoInterframe := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264,
		avframe.FrameTypeInterframe, 40, 80, avccP,
	)

	// Build AAC audio frame
	aacPayload := make([]byte, 50)
	for i := range aacPayload {
		aacPayload[i] = byte(i + 0x10)
	}
	audioFrame := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC,
		avframe.FrameTypeInterframe, 0, 0, aacPayload,
	)

	// Mux all frames
	var tsData []byte
	muxer.WriteFrame(videoSeqHeader)      // returns nil, parsed internally
	muxer.WriteFrame(audioSeqHeader)      // returns nil, parsed internally
	tsData = append(tsData, muxer.WriteFrame(videoKeyframe)...)
	tsData = append(tsData, muxer.WriteFrame(audioFrame)...)
	tsData = append(tsData, muxer.WriteFrame(videoInterframe)...)

	// Now demux
	var frames []*avframe.AVFrame
	dmx := NewDemuxer(func(f *avframe.AVFrame) {
		// Deep copy the frame to avoid buffer reuse issues
		cp := *f
		cp.Payload = make([]byte, len(f.Payload))
		copy(cp.Payload, f.Payload)
		frames = append(frames, &cp)
	})

	// Feed the last interframe data to trigger flushing of previous PES
	// We need to send another PES unit or EOF to flush the last accumulated PES.
	// Add a dummy video keyframe to trigger flush of the interframe PES.
	dummyKF := avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264,
		avframe.FrameTypeKeyframe, 80, 80, avccPayload,
	)
	tsData = append(tsData, muxer.WriteFrame(dummyKF)...)

	dmx.Feed(tsData)
	dmx.Flush()

	// Verify we got frames
	if len(frames) == 0 {
		t.Fatal("demuxer produced no frames")
	}

	// Check that we got video and audio frames
	var gotVideoSeq, gotAudioSeq, gotVideoKey, gotVideoInter, gotAudio bool
	for _, f := range frames {
		if f.MediaType.IsVideo() {
			if f.FrameType == avframe.FrameTypeSequenceHeader {
				gotVideoSeq = true
				if f.Codec != avframe.CodecH264 {
					t.Errorf("video seq header codec = %v, want H264", f.Codec)
				}
			} else if f.FrameType == avframe.FrameTypeKeyframe {
				gotVideoKey = true
				if f.DTS != 0 && f.DTS != 80 {
					t.Errorf("unexpected keyframe DTS: %d", f.DTS)
				}
			} else if f.FrameType == avframe.FrameTypeInterframe {
				gotVideoInter = true
				if f.DTS != 40 {
					t.Errorf("interframe DTS = %d, want 40", f.DTS)
				}
				if f.PTS != 80 {
					t.Errorf("interframe PTS = %d, want 80", f.PTS)
				}
			}
		}
		if f.MediaType.IsAudio() {
			if f.FrameType == avframe.FrameTypeSequenceHeader {
				gotAudioSeq = true
				if f.Codec != avframe.CodecAAC {
					t.Errorf("audio seq header codec = %v, want AAC", f.Codec)
				}
			} else {
				gotAudio = true
				if f.Codec != avframe.CodecAAC {
					t.Errorf("audio codec = %v, want AAC", f.Codec)
				}
			}
		}
	}

	if !gotVideoSeq {
		t.Error("missing video sequence header")
	}
	if !gotAudioSeq {
		t.Error("missing audio sequence header")
	}
	if !gotVideoKey {
		t.Error("missing video keyframe")
	}
	if !gotVideoInter {
		t.Error("missing video interframe")
	}
	if !gotAudio {
		t.Error("missing audio frame")
	}
}

func TestDemuxerPartialPackets(t *testing.T) {
	// Build some TS data
	pat := BuildPAT(0)
	pmt := BuildPMT(0x1B, 0x0F, 0)
	tsData := append(pat, pmt...)

	// Feed in small chunks
	dmx := NewDemuxer(func(f *avframe.AVFrame) {})
	chunkSize := 50
	for i := 0; i < len(tsData); i += chunkSize {
		end := i + chunkSize
		if end > len(tsData) {
			end = len(tsData)
		}
		dmx.Feed(tsData[i:end])
	}

	if dmx.pmtPID != PIDPmt {
		t.Errorf("PMT PID = 0x%04X, want 0x%04X", dmx.pmtPID, PIDPmt)
	}
	if dmx.videoCodec != avframe.CodecH264 {
		t.Errorf("video codec = %v, want H264", dmx.videoCodec)
	}
}

func TestDemuxerMP3Audio(t *testing.T) {
	// Build TS data with MP3 audio — need PAT/PMT first
	pat := BuildPAT(0)
	pmt := BuildPMT(0x1B, 0x03, 0) // H.264 video + MP3 audio

	muxer := NewMuxer(avframe.CodecH264, avframe.CodecMP3, nil, nil)

	mp3Payload := make([]byte, 100)
	mp3Payload[0] = 0xFF // MP3 sync
	mp3Payload[1] = 0xFB

	frame := avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecMP3,
		avframe.FrameTypeInterframe, 0, 0, mp3Payload,
	)

	var tsData []byte
	tsData = append(tsData, pat...)
	tsData = append(tsData, pmt...)
	tsData = append(tsData, muxer.WriteFrame(frame)...)
	// Need another audio frame to flush
	tsData = append(tsData, muxer.WriteFrame(frame)...)

	var frames []*avframe.AVFrame
	dmx := NewDemuxer(func(f *avframe.AVFrame) {
		cp := *f
		cp.Payload = make([]byte, len(f.Payload))
		copy(cp.Payload, f.Payload)
		frames = append(frames, &cp)
	})
	dmx.Feed(tsData)
	dmx.Flush()

	gotMP3 := false
	for _, f := range frames {
		if f.MediaType.IsAudio() && f.Codec == avframe.CodecMP3 {
			gotMP3 = true
		}
	}
	if !gotMP3 {
		t.Error("missing MP3 audio frame")
	}
}

func TestDemuxerSyncRecovery(t *testing.T) {
	pat := BuildPAT(0)
	// Prepend garbage bytes
	garbage := []byte{0x00, 0x01, 0x02, 0x03, 0x04}
	data := append(garbage, pat...)

	dmx := NewDemuxer(func(f *avframe.AVFrame) {})
	dmx.Feed(data)

	if dmx.pmtPID != PIDPmt {
		t.Errorf("failed to recover sync: PMT PID = 0x%04X, want 0x%04X", dmx.pmtPID, PIDPmt)
	}
}

func TestDemuxerADTSRoundTrip(t *testing.T) {
	// Manually build ADTS + AAC data to test the ADTS parsing path
	info := &aac.AACInfo{ObjectType: 2, SampleRate: 44100, Channels: 2}
	rawAAC := make([]byte, 50)
	for i := range rawAAC {
		rawAAC[i] = byte(i)
	}
	adtsHeader := aac.BuildADTSHeader(info, len(rawAAC))
	adtsFrame := append(adtsHeader, rawAAC...)

	// Build PES manually with this ADTS data
	pesHeader := BuildPESHeader(0xC0, 100, 100, len(adtsFrame))
	pesData := append(pesHeader, adtsFrame...)

	// Packetize into TS on audio PID
	var cc uint8
	tsPackets := PacketizePES(PIDAudio, pesData, &cc, nil)

	// We need PAT + PMT first, then audio, then another audio to flush
	pat := BuildPAT(0)
	pmt := BuildPMT(0x1B, 0x0F, 0) // video H.264, audio AAC

	var tsData []byte
	tsData = append(tsData, pat...)
	tsData = append(tsData, pmt...)
	tsData = append(tsData, tsPackets...)

	// Build a second ADTS frame to trigger flush of first
	pesHeader2 := BuildPESHeader(0xC0, 200, 200, len(adtsFrame))
	pesData2 := append(pesHeader2, adtsFrame...)
	tsPackets2 := PacketizePES(PIDAudio, pesData2, &cc, nil)
	tsData = append(tsData, tsPackets2...)

	var frames []*avframe.AVFrame
	dmx := NewDemuxer(func(f *avframe.AVFrame) {
		cp := *f
		cp.Payload = make([]byte, len(f.Payload))
		copy(cp.Payload, f.Payload)
		frames = append(frames, &cp)
	})
	dmx.Feed(tsData)

	// Should have audio seq header + audio frame
	var gotSeq, gotAudio bool
	for _, f := range frames {
		if f.MediaType.IsAudio() && f.FrameType == avframe.FrameTypeSequenceHeader {
			gotSeq = true
		} else if f.MediaType.IsAudio() && f.Codec == avframe.CodecAAC {
			gotAudio = true
			// Verify the payload matches the raw AAC (ADTS stripped)
			if len(f.Payload) != len(rawAAC) {
				t.Errorf("AAC payload length = %d, want %d", len(f.Payload), len(rawAAC))
			}
		}
	}
	if !gotSeq {
		t.Error("missing AAC sequence header")
	}
	if !gotAudio {
		t.Error("missing AAC audio frame")
	}
}

// TestDemuxerBufferAliasing verifies that frame payloads are independent copies,
// not subslices of the reused PES accumulation buffer. Earlier frame data must
// survive after subsequent PES packets are processed.
func TestDemuxerBufferAliasing(t *testing.T) {
	info := &aac.AACInfo{ObjectType: 2, SampleRate: 44100, Channels: 2}

	// Create two distinct AAC payloads
	rawAAC1 := make([]byte, 30)
	for i := range rawAAC1 {
		rawAAC1[i] = byte(i + 0xA0)
	}
	rawAAC2 := make([]byte, 30)
	for i := range rawAAC2 {
		rawAAC2[i] = byte(i + 0xB0)
	}

	adts1 := append(aac.BuildADTSHeader(info, len(rawAAC1)), rawAAC1...)
	adts2 := append(aac.BuildADTSHeader(info, len(rawAAC2)), rawAAC2...)

	var cc uint8
	pat := BuildPAT(0)
	pmt := BuildPMT(0x1B, 0x0F, 0)

	pes1 := append(BuildPESHeader(0xC0, 100, 100, len(adts1)), adts1...)
	pes2 := append(BuildPESHeader(0xC0, 200, 200, len(adts2)), adts2...)
	// Third PES to flush pes2
	pes3 := append(BuildPESHeader(0xC0, 300, 300, len(adts1)), adts1...)

	var tsData []byte
	tsData = append(tsData, pat...)
	tsData = append(tsData, pmt...)
	tsData = append(tsData, PacketizePES(PIDAudio, pes1, &cc, nil)...)
	tsData = append(tsData, PacketizePES(PIDAudio, pes2, &cc, nil)...)
	tsData = append(tsData, PacketizePES(PIDAudio, pes3, &cc, nil)...)

	// Collect frames WITHOUT copying — this simulates what stream.WriteFrame() does
	// (stores the AVFrame pointer directly).
	var frames []*avframe.AVFrame
	dmx := NewDemuxer(func(f *avframe.AVFrame) {
		frames = append(frames, f)
	})
	dmx.Feed(tsData)
	dmx.Flush()

	// Find the raw AAC frames (skip sequence headers)
	var audioFrames []*avframe.AVFrame
	for _, f := range frames {
		if f.MediaType.IsAudio() && f.FrameType != avframe.FrameTypeSequenceHeader {
			audioFrames = append(audioFrames, f)
		}
	}

	if len(audioFrames) < 2 {
		t.Fatalf("expected at least 2 audio frames, got %d", len(audioFrames))
	}

	// Verify first frame's payload was not corrupted by second frame's PES
	for i, b := range audioFrames[0].Payload {
		expected := byte(i + 0xA0)
		if b != expected {
			t.Errorf("frame[0].Payload[%d] = 0x%02X, want 0x%02X (buffer aliasing corruption)", i, b, expected)
			break
		}
	}

	for i, b := range audioFrames[1].Payload {
		expected := byte(i + 0xB0)
		if b != expected {
			t.Errorf("frame[1].Payload[%d] = 0x%02X, want 0x%02X (buffer aliasing corruption)", i, b, expected)
			break
		}
	}
}
