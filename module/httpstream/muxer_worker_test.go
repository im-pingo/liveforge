package httpstream

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/h265"
	"github.com/im-pingo/liveforge/pkg/muxer/flv"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

type muxerWorkerPublisher struct {
	info *avframe.MediaInfo
}

func (p *muxerWorkerPublisher) ID() string                    { return "muxer-worker-publisher" }
func (p *muxerWorkerPublisher) MediaInfo() *avframe.MediaInfo { return p.info }
func (p *muxerWorkerPublisher) Close() error                  { return nil }

func newMuxerWorkerStream(t *testing.T, audioCodec avframe.CodecType) *core.Stream {
	t.Helper()
	stream := core.NewStream("live/muxer-worker", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 256,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&muxerWorkerPublisher{info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
		AudioCodec: audioCodec,
		SampleRate: 48000,
		Channels:   2,
	}}); err != nil {
		t.Fatal(err)
	}

	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x42, 0x00, 0x1e, 0xff},
	))
	if audioCodec == avframe.CodecOpus {
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeSequenceHeader,
			0, 0, []byte("OpusHead\x01\x02\x38\x01\x80\xbb\x00\x00\x00\x00\x00"),
		))
	} else if audioCodec == avframe.CodecAAC {
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
			0, 0, []byte{0x12, 0x10},
		))
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0, 0, 0, 2, 0x65, 0x01},
	))
	return stream
}

func newMuxerWorkerInstance(stream *core.Stream) (*core.MuxerInstance, *core.SharedBufferReader) {
	inst := &core.MuxerInstance{
		Buffer:     core.NewSharedBuffer(256),
		Generation: stream.StartupSnapshot().Generation,
		Done:       make(chan struct{}),
	}
	return inst, inst.Buffer.NewReader()
}

func newPreReadyMuxerWorkerStream(t *testing.T) *core.Stream {
	t.Helper()
	stream := core.NewStream("live/pre-ready-muxer-worker", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 256,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&muxerWorkerPublisher{info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH264,
	}}); err != nil {
		t.Fatal(err)
	}
	if stream.StartupSnapshot().Ready {
		t.Fatal("test publisher unexpectedly started ready")
	}
	return stream
}

func waitForMuxerInit(t *testing.T, inst *core.MuxerInstance) []byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if initData := inst.InitData(); initData != nil {
			return append([]byte(nil), initData...)
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("muxer did not publish init data")
	return nil
}

func collectMuxerOutput(reader *core.SharedBufferReader) []byte {
	var output []byte
	for {
		packet, ok := reader.Read()
		if !ok {
			return output
		}
		output = append(output, packet...)
	}
}

func TestFLVMuxerDropsOpusWhenAudioTranscodingIsUnavailable(t *testing.T) {
	stream := newMuxerWorkerStream(t, avframe.CodecOpus)
	inst, reader := newMuxerWorkerInstance(stream)
	workerDone := make(chan struct{})
	go func() {
		new(Module).runFLVMuxer(inst, stream)
		close(workerDone)
	}()

	initData := waitForMuxerInit(t, inst)
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe,
		20, 20, []byte{0xf8, 0xff, 0xfe},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		33, 33, []byte{0, 0, 0, 2, 0x41, 0x01},
	))
	stream.RingBuffer().Close()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("FLV muxer did not stop after source closed")
	}
	close(inst.Done)

	if len(initData) < 5 || initData[4]&0x04 != 0 {
		t.Fatalf("FLV header flags = 0x%02x, want video-only output", initData[4])
	}
	output := append(initData, collectMuxerOutput(reader)...)
	demuxer := flv.NewDemuxer(bytes.NewReader(output))
	for {
		frame, err := demuxer.ReadTag()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("demux FLV output: %v", err)
		}
		if frame != nil && frame.MediaType.IsAudio() {
			t.Fatalf("FLV output contains unsupported audio codec %s", frame.Codec)
		}
	}
}

func TestFLVMuxerStopsOnPublisherGenerationEnd(t *testing.T) {
	stream := newMuxerWorkerStream(t, 0)
	inst, _ := newMuxerWorkerInstance(stream)
	workerDone := make(chan struct{})
	go func() {
		new(Module).runFLVMuxer(inst, stream)
		close(workerDone)
	}()
	waitForMuxerInit(t, inst)

	stream.RemovePublisher()
	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("FLV muxer did not stop when its publisher generation ended")
	}
}

func TestFLVMuxerStopsWhenPreReadyGenerationIsRemoved(t *testing.T) {
	stream := newPreReadyMuxerWorkerStream(t)
	inst, _ := newMuxerWorkerInstance(stream)
	workerDone := make(chan struct{})
	go func() {
		new(Module).runFLVMuxer(inst, stream)
		close(workerDone)
	}()

	stream.RemovePublisher()
	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("FLV muxer remained blocked after its pre-ready publisher generation ended")
	}
}

func TestFLVMuxerDoesNotAttachToReplacementAfterPreReadyGenerationEnds(t *testing.T) {
	stream := newPreReadyMuxerWorkerStream(t)
	inst, reader := newMuxerWorkerInstance(stream)
	workerDone := make(chan struct{})
	go func() {
		new(Module).runFLVMuxer(inst, stream)
		close(workerDone)
	}()

	stream.RemovePublisher()
	replacement := &muxerWorkerPublisher{info: &avframe.MediaInfo{
		VideoCodec:          avframe.CodecH264,
		VideoSequenceHeader: []byte{0x01, 0x42, 0x00, 0x1e, 0xff},
	}}
	if err := stream.SetPublisher(replacement); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeKeyframe,
		0, 0, []byte{0, 0, 0, 2, 0x65, 0x02},
	))

	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("old FLV muxer did not terminate after publisher replacement")
	}
	if initData := inst.InitData(); initData != nil {
		t.Fatalf("old FLV muxer published %d bytes of replacement init data", len(initData))
	}
	if data, ok := reader.TryRead(); ok {
		t.Fatalf("old FLV muxer published %d bytes from replacement generation", len(data))
	}
}

func TestTSMuxerDropsOpusWhenAudioTranscodingIsUnavailable(t *testing.T) {
	stream := newMuxerWorkerStream(t, avframe.CodecOpus)
	inst, reader := newMuxerWorkerInstance(stream)
	workerDone := make(chan struct{})
	go func() {
		new(Module).runTSMuxer(inst, stream)
		close(workerDone)
	}()
	firstPacket := make(chan []byte, 1)
	go func() {
		packet, _ := reader.Read()
		firstPacket <- packet
	}()
	var output []byte
	select {
	case packet := <-firstPacket:
		output = append(output, packet...)
	case <-time.After(2 * time.Second):
		t.Fatal("TS muxer did not publish the cached video GOP")
	}

	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe,
		20, 20, []byte{0xf8, 0xff, 0xfe},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeInterframe,
		33, 33, []byte{0, 0, 0, 2, 0x41, 0x01},
	))
	stream.RingBuffer().Close()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("TS muxer did not stop after source closed")
	}
	close(inst.Done)

	output = append(output, collectMuxerOutput(reader)...)
	if len(output) == 0 {
		t.Fatal("TS muxer produced no video output")
	}
	for offset := 0; offset+ts.PacketSize <= len(output); offset += ts.PacketSize {
		packet := output[offset : offset+ts.PacketSize]
		if packet[0] != ts.SyncByte {
			t.Fatalf("TS packet at offset %d has sync byte 0x%02x", offset, packet[0])
		}
		pid := uint16(packet[1]&0x1f)<<8 | uint16(packet[2])
		if pid == ts.PIDAudio {
			t.Fatal("TS output contains an audio PID for unsupported Opus")
		}
	}
}

func TestTSMuxerAnnouncesLateAACBeforeFirstAudioFrame(t *testing.T) {
	stream := newMuxerWorkerStream(t, 0)
	inst, reader := newMuxerWorkerInstance(stream)
	workerDone := make(chan struct{})
	go func() {
		new(Module).runTSMuxer(inst, stream)
		close(workerDone)
	}()
	readMuxerPacket(t, reader) // Cached video GOP.

	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		20, 20, []byte{0x12, 0x10},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
		40, 40, []byte{1, 2},
	))
	announcement := readMuxerPacket(t, reader)
	if !tsOutputDeclaresStreamType(announcement, 0x0f) {
		t.Fatal("HTTP TS output did not announce the late AAC track before its first media frame")
	}
	media := readMuxerPacket(t, reader)
	if !tsOutputContainsPID(media, ts.PIDAudio) {
		t.Fatal("HTTP TS output dropped the first late-track audio frame")
	}
	time.Sleep(50 * time.Millisecond)
	if data, ok := reader.TryRead(); ok {
		t.Fatalf("HTTP TS output duplicated the first late-track audio frame in %d extra bytes", len(data))
	}

	stream.RingBuffer().Close()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("TS muxer did not stop after source closed")
	}
	close(inst.Done)
}

func TestFMP4MuxerSynthesizesWHIPOpusTrackConfiguration(t *testing.T) {
	stream := newMuxerWorkerStream(t, avframe.CodecOpus)
	inst, _ := newMuxerWorkerInstance(stream)
	workerDone := make(chan struct{})
	go func() {
		new(Module).runFMP4Muxer(inst, stream)
		close(workerDone)
	}()

	initData := waitForMuxerInit(t, inst)
	stream.RingBuffer().Close()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("FMP4 muxer did not stop after source closed")
	}
	close(inst.Done)

	if !bytes.Contains(initData, []byte("Opus")) {
		t.Fatal("FMP4 init segment does not declare an Opus sample entry")
	}
	if !bytes.Contains(initData, []byte("dOps")) {
		t.Fatal("FMP4 Opus sample entry is missing its dOps configuration box")
	}

	firstMDHD := bytes.Index(initData, []byte("mdhd"))
	if firstMDHD < 0 {
		t.Fatal("FMP4 init segment is missing the video mdhd box")
	}
	secondOffset := firstMDHD + 4
	secondMDHDRel := bytes.Index(initData[secondOffset:], []byte("mdhd"))
	if secondMDHDRel < 0 {
		t.Fatal("FMP4 init segment is missing the audio mdhd box")
	}
	secondMDHD := secondOffset + secondMDHDRel
	if secondMDHD+20 > len(initData) {
		t.Fatal("FMP4 audio mdhd box is truncated")
	}
	if sampleRate := binary.BigEndian.Uint32(initData[secondMDHD+16 : secondMDHD+20]); sampleRate != 48000 {
		t.Fatalf("FMP4 Opus timescale = %d, want 48000", sampleRate)
	}
}

func TestFMP4MuxerDerivesH265TrackDimensions(t *testing.T) {
	stream := newH265MuxerWorkerStream(t)
	inst, _ := newMuxerWorkerInstance(stream)
	workerDone := make(chan struct{})
	go func() {
		new(Module).runFMP4Muxer(inst, stream)
		close(workerDone)
	}()

	initData := waitForMuxerInit(t, inst)
	stream.RingBuffer().Close()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("FMP4 muxer did not stop after source closed")
	}
	close(inst.Done)

	assertH265SampleEntryDimensions(t, initData, 640, 480)
}

func TestFMP4MuxerBatchesLiveAudioAndVideoIntoSameFragment(t *testing.T) {
	stream := newH265MuxerWorkerStream(t)
	inst, reader := newMuxerWorkerInstance(stream)
	workerDone := make(chan struct{})
	go func() {
		new(Module).runFMP4Muxer(inst, stream)
		close(workerDone)
	}()

	initData := waitForMuxerInit(t, inst)
	demuxer, err := fmp4.NewDemuxer(initData)
	if err != nil {
		t.Fatalf("create fMP4 demuxer: %v", err)
	}

	// Consume the cached GOP fragment. Live A/V frames after this point must be
	// grouped into a multiplexed fragment instead of alternating single-track
	// fragments, which create timestamp gaps in an MSE sequence SourceBuffer.
	readMuxerPacket(t, reader)
	for dts := int64(20); dts <= 220; dts += 20 {
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeInterframe,
			dts, dts, []byte{0xf8, 0xff, 0xfe},
		))
		if dts%40 == 0 {
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe,
				dts, dts, []byte{0, 0, 0, 2, 0x02, 0x01},
			))
		}
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe,
		240, 240, []byte{0, 0, 0, 2, 0x02, 0x01},
	))

	liveFragment := readMuxerPacket(t, reader)
	frames, err := demuxer.Parse(liveFragment)
	if err != nil {
		t.Fatalf("demux live fMP4 fragment: %v", err)
	}
	var videoFrames, audioFrames int
	for _, frame := range frames {
		if frame.MediaType.IsVideo() {
			videoFrames++
		}
		if frame.MediaType.IsAudio() {
			audioFrames++
		}
	}
	if videoFrames == 0 || audioFrames == 0 {
		t.Fatalf("live fragment tracks: video=%d audio=%d, want both tracks", videoFrames, audioFrames)
	}

	stream.RingBuffer().Close()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("FMP4 muxer did not stop after source closed")
	}
	close(inst.Done)
}

func TestFMP4MuxerRebasesH265AACBFramesOntoSubscriberTimeline(t *testing.T) {
	const sourceBaseDTS = int64(591600)
	stream := newH265MuxerWorkerStreamAt(t, avframe.CodecAAC, sourceBaseDTS)
	for elapsed := int64(0); elapsed <= 160; elapsed += 20 {
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
			sourceBaseDTS+elapsed, sourceBaseDTS+elapsed, []byte{0x21, 0x10},
		))
		if elapsed > 0 && elapsed%40 == 0 {
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe,
				sourceBaseDTS+elapsed, sourceBaseDTS+elapsed, []byte{0, 0, 0, 2, 0x02, 0x01},
			))
		}
	}

	inst, reader := newMuxerWorkerInstance(stream)
	workerDone := make(chan struct{})
	go func() {
		new(Module).runFMP4Muxer(inst, stream)
		close(workerDone)
	}()

	demuxer, err := fmp4.NewDemuxer(waitForMuxerInit(t, inst))
	if err != nil {
		t.Fatalf("create fMP4 demuxer: %v", err)
	}
	cachedFrames, err := demuxer.Parse(readMuxerPacket(t, reader))
	if err != nil {
		t.Fatalf("demux cached fMP4 fragment: %v", err)
	}

	for elapsed := int64(180); elapsed <= 400; elapsed += 20 {
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeInterframe,
			sourceBaseDTS+elapsed, sourceBaseDTS+elapsed, []byte{0x21, 0x10},
		))
		if elapsed%40 == 0 {
			stream.WriteFrame(avframe.NewAVFrame(
				avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeInterframe,
				sourceBaseDTS+elapsed, sourceBaseDTS+elapsed-40, []byte{0, 0, 0, 2, 0x02, 0x01},
			))
		}
	}
	liveFrames, err := demuxer.Parse(readMuxerPacket(t, reader))
	if err != nil {
		t.Fatalf("demux live fMP4 fragment: %v", err)
	}

	assertFMP4TrackTimeline(t, cachedFrames, avframe.MediaTypeVideo,
		[]int64{0, 40, 80, 120, 160}, []int64{0, 40, 80, 120, 160})
	assertFMP4TrackTimeline(t, cachedFrames, avframe.MediaTypeAudio,
		[]int64{0, 20, 40, 60, 80, 100, 120, 140, 160}, []int64{0, 20, 40, 60, 80, 100, 120, 140, 160})
	assertFMP4TrackTimeline(t, liveFrames, avframe.MediaTypeVideo,
		[]int64{200, 240, 280, 320, 360}, []int64{160, 200, 240, 280, 320})
	assertFMP4TrackTimeline(t, liveFrames, avframe.MediaTypeAudio,
		[]int64{180, 200, 220, 240, 260, 280, 300, 320, 340, 360, 380, 400},
		[]int64{180, 200, 220, 240, 260, 280, 300, 320, 340, 360, 380, 400})

	stream.RingBuffer().Close()
	select {
	case <-workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("FMP4 muxer did not stop after source closed")
	}
	close(inst.Done)
}

func assertFMP4TrackTimeline(t *testing.T, frames []*avframe.AVFrame, mediaType avframe.MediaType, wantDTS, wantPTS []int64) {
	t.Helper()
	var gotDTS, gotPTS []int64
	for _, frame := range frames {
		if frame.MediaType != mediaType {
			continue
		}
		gotDTS = append(gotDTS, frame.DTS)
		gotPTS = append(gotPTS, frame.PTS)
	}
	if len(gotDTS) != len(wantDTS) {
		t.Fatalf("%v frame count = %d, want %d (DTS %v)", mediaType, len(gotDTS), len(wantDTS), gotDTS)
	}
	for i := range wantDTS {
		if gotDTS[i] != wantDTS[i] || gotPTS[i] != wantPTS[i] {
			t.Fatalf("%v frame %d timing = DTS %d PTS %d, want DTS %d PTS %d",
				mediaType, i, gotDTS[i], gotPTS[i], wantDTS[i], wantPTS[i])
		}
	}
}

func readMuxerPacket(t *testing.T, reader *core.SharedBufferReader) []byte {
	t.Helper()
	result := make(chan []byte, 1)
	go func() {
		packet, _ := reader.Read()
		result <- packet
	}()
	select {
	case packet := <-result:
		if len(packet) == 0 {
			t.Fatal("muxer returned an empty packet")
		}
		return packet
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for muxer packet")
		return nil
	}
}

func tsOutputDeclaresStreamType(data []byte, streamType byte) bool {
	for offset := 0; offset+ts.PacketSize <= len(data); offset += ts.PacketSize {
		pkt := data[offset : offset+ts.PacketSize]
		pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
		if pid != ts.PIDPmt || pkt[1]&0x40 == 0 {
			continue
		}
		pos := 4
		if pkt[3]&0x20 != 0 {
			pos += 1 + int(pkt[4])
		}
		if pos >= len(pkt) {
			continue
		}
		pos += 1 + int(pkt[pos])
		if pos+12 > len(pkt) {
			continue
		}
		sectionLen := int(pkt[pos+1]&0x0f)<<8 | int(pkt[pos+2])
		end := pos + 3 + sectionLen - 4
		programInfoLen := int(pkt[pos+10]&0x0f)<<8 | int(pkt[pos+11])
		for i := pos + 12 + programInfoLen; i+4 < end && i+4 < len(pkt); {
			if pkt[i] == streamType {
				return true
			}
			i += 5 + int(pkt[i+3]&0x0f)<<8 + int(pkt[i+4])
		}
	}
	return false
}

func tsOutputContainsPID(data []byte, wantPID uint16) bool {
	for offset := 0; offset+ts.PacketSize <= len(data); offset += ts.PacketSize {
		pkt := data[offset : offset+ts.PacketSize]
		pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
		if pid == wantPID {
			return true
		}
	}
	return false
}

func newH265MuxerWorkerStream(t *testing.T) *core.Stream {
	return newH265MuxerWorkerStreamAt(t, avframe.CodecOpus, 0)
}

func newH265MuxerWorkerStreamAt(t *testing.T, audioCodec avframe.CodecType, baseDTS int64) *core.Stream {
	t.Helper()
	stream := core.NewStream("live/muxer-worker-h265", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 256,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&muxerWorkerPublisher{info: &avframe.MediaInfo{
		VideoCodec: avframe.CodecH265,
		AudioCodec: audioCodec,
		SampleRate: 48000,
		Channels:   2,
	}}); err != nil {
		t.Fatal(err)
	}

	sps, err := hex.DecodeString("420103016000000300b0000003000003005a0000a0050201e162023b914842e7e13d0bea1bd50feaa08f554a6a02020207f08041")
	if err != nil {
		t.Fatal(err)
	}
	var annexB []byte
	for _, nal := range [][]byte{
		{0x40, 0x01, 0x0c, 0x03, 0xff, 0xff, 0x01, 0x60},
		sps,
		{0x44, 0x01, 0xc0, 0x72, 0xf0, 0x5b, 0x24},
	} {
		annexB = append(annexB, 0, 0, 0, 1)
		annexB = append(annexB, nal...)
	}
	sequenceHeader := h265.BuildHVCCDecoderConfig(annexB)
	if sequenceHeader == nil {
		t.Fatal("failed to build H.265 sequence header")
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeSequenceHeader,
		0, 0, sequenceHeader,
	))
	if audioCodec == avframe.CodecAAC {
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
			0, 0, []byte{0x11, 0x90},
		))
	}
	if audioCodec == avframe.CodecOpus {
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio, avframe.CodecOpus, avframe.FrameTypeSequenceHeader,
			0, 0, []byte("OpusHead\x01\x02\x38\x01\x80\xbb\x00\x00\x00\x00\x00"),
		))
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH265, avframe.FrameTypeKeyframe,
		baseDTS, baseDTS, []byte{0, 0, 0, 2, 0x26, 0x01},
	))
	return stream
}

func assertH265SampleEntryDimensions(t *testing.T, initData []byte, wantWidth, wantHeight uint16) {
	t.Helper()
	hvc1 := bytes.Index(initData, []byte("hvc1"))
	if hvc1 < 0 || hvc1+32 > len(initData) {
		t.Fatal("FMP4 init segment is missing a complete hvc1 sample entry")
	}
	width := binary.BigEndian.Uint16(initData[hvc1+28 : hvc1+30])
	height := binary.BigEndian.Uint16(initData[hvc1+30 : hvc1+32])
	if width != wantWidth || height != wantHeight {
		t.Fatalf("FMP4 H.265 sample entry dimensions = %dx%d, want %dx%d", width, height, wantWidth, wantHeight)
	}
}
