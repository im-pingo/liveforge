//go:build audiocodec

package dvr

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

func TestDVRSessionTranscodesG711AudioToAAC(t *testing.T) {
	dir := t.TempDir()
	stream := core.NewStream("sip/dvr-g711", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 256,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	if err := stream.SetPublisher(&dvrStaleHistoryPublisher{
		id: "sip-dvr-g711",
		info: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecG711A,
			SampleRate: 8000,
			Channels:   1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 256))

	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH264,
		avframe.FrameTypeSequenceHeader,
		0,
		0,
		dvrTestAVCConfig(),
	))
	stream.WriteFrame(labmedia.VideoFrame(0))
	session, err := NewSession("sip/dvr-g711", stream, config.DVRConfig{
		Path:            filepath.Join(dir, "{stream_key}"),
		SegmentDuration: time.Second,
	}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	done := make(chan struct{})
	go func() {
		session.Run()
		close(done)
	}()
	for dts := int64(0); dts < 1200; dts += labmedia.VideoFrameDurationMs {
		stream.WriteFrame(labmedia.VideoFrame(dts))
		stream.WriteFrame(labmedia.G711Frame(avframe.CodecG711A, dts))
	}
	time.Sleep(150 * time.Millisecond)
	session.Stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("DVR G.711 session did not stop")
	}

	segments := session.Index().Segments()
	if len(segments) == 0 {
		t.Fatal("DVR did not write any segments")
	}
	var audioFrames int
	for _, segment := range segments {
		data, readErr := os.ReadFile(segment.DiskPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
			if frame.MediaType.IsAudio() {
				audioFrames++
				if frame.Codec != avframe.CodecAAC {
					t.Errorf("DVR audio codec = %s, want AAC", frame.Codec)
				}
			}
		})
		demuxer.Feed(data)
		demuxer.Flush()
	}
	if audioFrames == 0 {
		t.Fatal("DVR TS contained no demuxable AAC audio frames")
	}
}

func TestDVRSessionDrainsTranscodedGenerationTail(t *testing.T) {
	dir := t.TempDir()
	stream := core.NewStream("sip/dvr-g711-tail", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 256,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	pub := &dvrStaleHistoryPublisher{
		id: "sip-dvr-g711-tail",
		info: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecG711A,
			SampleRate: 8000,
			Channels:   1,
		},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 256))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, dvrTestAVCConfig(),
	))
	stream.WriteFrame(labmedia.VideoFrame(0))

	session, err := NewSession("sip/dvr-g711-tail", stream, config.DVRConfig{
		Path:            filepath.Join(dir, "{stream_key}"),
		SegmentDuration: time.Second,
	}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	for dts := labmedia.VideoFrameDurationMs; dts < 400; dts += labmedia.VideoFrameDurationMs {
		stream.WriteFrame(labmedia.VideoFrame(dts))
		stream.WriteFrame(labmedia.G711Frame(avframe.CodecG711A, dts))
	}
	stream.RemovePublisherIf(pub)
	session.Run()

	var videoFrames, audioFrames int
	for _, segment := range session.Index().Segments() {
		data, readErr := os.ReadFile(segment.DiskPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
			if frame.FrameType == avframe.FrameTypeSequenceHeader {
				return
			}
			if frame.MediaType.IsVideo() {
				videoFrames++
			} else if frame.MediaType.IsAudio() {
				audioFrames++
				if frame.Codec != avframe.CodecAAC {
					t.Errorf("DVR tail audio codec = %s, want AAC", frame.Codec)
				}
			}
		})
		demuxer.Feed(data)
		demuxer.Flush()
	}
	if videoFrames < 2 || audioFrames == 0 {
		t.Fatalf("drained transcoded tail frames: video=%d audio=%d, want video>=2 and audio>0", videoFrames, audioFrames)
	}
}

type blockingAudioSegment struct {
	segmentFile
	once    sync.Once
	entered chan<- struct{}
	release <-chan struct{}
}

func (f *blockingAudioSegment) Write(p []byte) (int, error) {
	f.once.Do(func() {
		close(f.entered)
		<-f.release
	})
	return f.segmentFile.Write(p)
}

func TestDVRImmediateReplacementPreservesBoundTranscodedPayloadSequence(t *testing.T) {
	dir := t.TempDir()
	stream := core.NewStream("sip/dvr-partial-audio-tail", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 32,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	pub := &dvrStaleHistoryPublisher{
		id: "sip-dvr-partial-audio-tail",
		info: &avframe.MediaInfo{
			AudioCodec: avframe.CodecG711A,
			SampleRate: 8000,
			Channels:   1,
		},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	tm := core.NewTranscodeManager(stream, audiocodec.Global(), 64)
	core.SetTranscodeManagerForTest(stream, tm)
	session, err := NewSession("sip/dvr-partial-audio-tail", stream, config.DVRConfig{
		Path:            filepath.Join(dir, "{stream_key}"),
		SegmentDuration: time.Second,
	}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	expectedReader, releaseExpected, err := tm.GetOrCreateReaderAtFromHistory(avframe.CodecAAC, stream.StartupSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	defer expectedReader.Close()
	defer releaseExpected()

	entered := make(chan struct{})
	releaseWriter := make(chan struct{})
	var releaseOnce sync.Once
	unblockWriter := func() { releaseOnce.Do(func() { close(releaseWriter) }) }
	defer unblockWriter()
	session.wrapSegment = func(file segmentFile) segmentFile {
		return &blockingAudioSegment{segmentFile: file, entered: entered, release: releaseWriter}
	}
	done := make(chan struct{})
	go func() {
		session.Run()
		close(done)
	}()

	for i := range 8 {
		dts := int64(1000 + i*20)
		stream.WriteFrameForPublisher(pub, labmedia.G711Frame(avframe.CodecG711A, dts))
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("DVR did not begin writing old-generation transcoded audio")
	}

	stream.WriteFrameForPublisher(pub, labmedia.G711Frame(avframe.CodecG711A, 1160))
	endCursor := stream.RingBuffer().WriteCursor()
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 3*time.Second)
	if !tm.WaitForSourceCursor(avframe.CodecAAC, endCursor, waitCtx) {
		cancelWait()
		t.Fatal("old-generation transcoder did not consume the final source frame")
	}
	cancelWait()
	if !stream.RemovePublisherIf(pub) {
		t.Fatal("old publisher was not removed")
	}
	if err := stream.SetPublisher(&dvrStaleHistoryPublisher{
		id: "sip/dvr-partial-audio-tail-replacement",
		info: &avframe.MediaInfo{
			AudioCodec: avframe.CodecG711A,
			SampleRate: 8000,
			Channels:   1,
		},
	}); err != nil {
		t.Fatalf("install immediate replacement publisher: %v", err)
	}
	unblockWriter()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("DVR did not finish the replaced old generation")
	}

	expected := readDVRAACPayloadsUntilClose(t, expectedReader)
	if len(expected) == 0 {
		t.Fatal("captured old-generation pipeline produced no AAC media")
	}

	var actual []*avframe.AVFrame
	for _, segment := range session.Index().Segments() {
		data, readErr := os.ReadFile(segment.DiskPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
			if frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader {
				actual = append(actual, frame)
			}
		})
		demuxer.Feed(data)
		demuxer.Flush()
	}
	if len(actual) != len(expected) {
		t.Fatalf("immediate-replacement DVR AAC frames = %d, want captured pipeline sequence length %d", len(actual), len(expected))
	}
	for i := range actual {
		if !bytes.Equal(actual[i].Payload, expected[i]) {
			t.Fatalf("immediate-replacement DVR AAC payload[%d] differs from captured pipeline output", i)
		}
		if i > 0 && actual[i].DTS <= actual[i-1].DTS {
			t.Fatalf("immediate-replacement DVR DTS[%d] = %d after %d, want strictly increasing", i, actual[i].DTS, actual[i-1].DTS)
		}
	}
}

func readDVRAACPayloadsUntilClose(t *testing.T, reader interface {
	ReadContext(context.Context) (*avframe.AVFrame, bool)
	TryRead() (*avframe.AVFrame, bool)
}) [][]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var payloads [][]byte
	for {
		frame, ok := reader.ReadContext(ctx)
		if !ok {
			if ctx.Err() != nil {
				t.Fatalf("captured transcode reader did not close: %v", ctx.Err())
			}
			break
		}
		if frame != nil && frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader {
			payloads = append(payloads, append([]byte(nil), frame.Payload...))
		}
	}
	if frame, ok := reader.TryRead(); ok {
		t.Fatalf("captured transcode reader returned extra frame after close: %+v", frame)
	}
	return payloads
}
