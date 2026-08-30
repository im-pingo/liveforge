//go:build audiocodec

package record

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/fmp4"
	"github.com/im-pingo/liveforge/pkg/util"
)

func TestFMP4RecordSessionTranscodesG711Audio(t *testing.T) {
	dir := t.TempDir()
	stream := core.NewStream("sip/record-g711", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 256,
	}, config.LimitsConfig{}, core.NewEventBus())
	if err := stream.SetPublisher(&testPublisher{
		id: "sip-record-g711",
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
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64, 0x00, 0x1f, 0xff},
	))

	session, err := NewRecordSession("sip/record-g711", stream, config.RecordConfig{
		Format: "fmp4",
		Path:   filepath.Join(dir, "{stream_key}", "{date}_{time}.mp4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		session.Run()
		close(done)
	}()

	for timestamp := int64(0); timestamp < 1200; timestamp += 20 {
		if timestamp%40 == 0 {
			stream.WriteFrame(labmedia.VideoFrame(timestamp))
		}
		stream.WriteFrame(labmedia.G711Frame(avframe.CodecG711A, timestamp))
	}
	session.Stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("G.711 fMP4 recording session did not stop")
	}

	if status := session.Status(); status.State != RecordingCompleted {
		t.Fatalf("G.711 fMP4 recording status = %+v, want completed", status)
	}
	data, err := os.ReadFile(session.writer.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("mp4a")) {
		t.Fatal("G.711 fMP4 recording did not contain the transcoded AAC track")
	}
}

func TestFMP4RecordSessionStopDrainsTranscodedInput(t *testing.T) {
	dir := t.TempDir()
	stream := core.NewStream("sip/record-g711-stop", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 256,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	if err := stream.SetPublisher(&testPublisher{
		id: "sip-record-g711-stop",
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
		avframe.MediaTypeVideo, avframe.CodecH264, avframe.FrameTypeSequenceHeader,
		0, 0, []byte{0x01, 0x64, 0x00, 0x1f, 0xff},
	))

	session, err := NewRecordSession("sip/record-g711-stop", stream, config.RecordConfig{
		Format: "fmp4",
		Path:   filepath.Join(dir, "{stream_key}", "{date}_{time}.mp4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for timestamp := int64(0); timestamp < 1200; timestamp += 20 {
		if timestamp%40 == 0 {
			stream.WriteFrame(labmedia.VideoFrame(timestamp))
		}
		stream.WriteFrame(labmedia.G711Frame(avframe.CodecG711A, timestamp))
	}
	// Stop before Run gets scheduled. The session must still drain the source
	// frames that were already written before the stop boundary.
	session.Stop()
	done := make(chan struct{})
	go func() {
		session.Run()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("G.711 fMP4 recording session did not stop")
	}

	if status := session.Status(); status.State != RecordingCompleted {
		t.Fatalf("G.711 stop-drain recording status = %+v, want completed", status)
	}
	data, err := os.ReadFile(session.writer.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("mp4a")) {
		t.Fatal("G.711 stop-drain recording did not contain the transcoded AAC track")
	}
}

func TestRecordSessionDrainsBoundTranscodedReaderAfterGenerationEnds(t *testing.T) {
	dir := t.TempDir()
	stream := core.NewStream("sip/record-generation-end", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 16,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	if err := stream.SetPublisher(&testPublisher{
		id: "sip-record-generation-end",
		info: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecG711A,
			SampleRate: 8000,
			Channels:   1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	session, err := NewRecordSession("sip/record-generation-end", stream, config.RecordConfig{
		Format: "fmp4",
		Path:   filepath.Join(dir, "{stream_key}", "{date}_{time}.mp4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.writer.Close()

	output := util.NewRingBuffer[*avframe.AVFrame](4)
	output.Write(avframe.NewAVFrame(
		avframe.MediaTypeAudio, avframe.CodecAAC, avframe.FrameTypeSequenceHeader,
		0, 0, audiocodec.Global().SequenceHeader(avframe.CodecAAC),
	))
	output.Close()
	session.reader = output.NewReader()
	session.transcoder = core.NewTranscodeManager(stream, audiocodec.Global(), 4)
	session.inputAudio = avframe.CodecAAC
	stream.RemovePublisher()

	if err := session.drainPendingFrames(); err != nil {
		t.Fatal(err)
	}
	if got := session.reader.ReadCursor(); got != 1 {
		t.Fatalf("transcoded reader cursor = %d, want pending old-generation frame consumed", got)
	}
}

func TestFMP4RecordSessionStartsAfterPublisherRemovalAndDrainsG711(t *testing.T) {
	dir := t.TempDir()
	stream := core.NewStream("sip/record-delayed-run", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 256,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	pub := &testPublisher{
		id: "sip-record-delayed-run",
		info: &avframe.MediaInfo{
			AudioCodec: avframe.CodecG711A,
			SampleRate: 8000,
			Channels:   1,
		},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	core.SetTranscodeManagerForTest(stream, core.NewTranscodeManager(stream, audiocodec.Global(), 256))
	session, err := NewRecordSession("sip/record-delayed-run", stream, config.RecordConfig{
		Format: "fmp4",
		Path:   filepath.Join(dir, "{stream_key}", "{date}_{time}.mp4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for timestamp := int64(0); timestamp < 1200; timestamp += 20 {
		stream.WriteFrameForPublisher(pub, labmedia.G711Frame(avframe.CodecG711A, timestamp))
	}
	stream.RemovePublisher()
	session.Stop()
	session.Run()

	if status := session.Status(); status.State != RecordingCompleted {
		t.Fatalf("delayed G.711 recording status = %+v, want completed", status)
	}
	data, err := os.ReadFile(session.writer.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("mp4a")) || !bytes.Contains(data, []byte("mdat")) {
		t.Fatal("delayed G.711 recording did not contain playable AAC media")
	}
}

func TestFMP4RecordGenerationEndPreservesPartialTranscodedAudioTailExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	stream := core.NewStream("sip/record-partial-audio-tail", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 32,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	pub := &testPublisher{
		id: "sip-record-partial-audio-tail",
		info: &avframe.MediaInfo{
			AudioCodec: avframe.CodecG711A,
			SampleRate: 8000,
			Channels:   1,
		},
	}
	if err := stream.SetPublisher(pub); err != nil {
		t.Fatal(err)
	}
	tm := core.NewTranscodeManager(stream, audiocodec.Global(), 32)
	core.SetTranscodeManagerForTest(stream, tm)
	session, err := NewRecordSession("sip/record-partial-audio-tail", stream, config.RecordConfig{
		Format: "fmp4",
		Path:   filepath.Join(dir, "{stream_key}", "{date}_{time}.mp4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	expectedReader, releaseExpected, err := tm.GetOrCreateReaderAtFromHistory(avframe.CodecAAC, stream.StartupSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	defer expectedReader.Close()
	defer releaseExpected()

	stream.WriteFrameForPublisher(pub, labmedia.G711Frame(avframe.CodecG711A, 1000))
	stream.RemovePublisherIf(pub)
	session.Run()
	if status := session.Status(); status.State != RecordingCompleted {
		t.Fatalf("partial-tail recording status = %+v, want completed", status)
	}
	data, err := os.ReadFile(session.writer.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	expected := readRecordAACPayloadsUntilClose(t, expectedReader)
	if len(expected) == 0 {
		t.Fatal("captured recording pipeline produced no AAC media")
	}
	demuxer, err := fmp4.NewDemuxer(data)
	if err != nil {
		t.Fatalf("parse partial-tail init segment: %v", err)
	}
	frames, err := demuxer.Parse(data)
	if err != nil {
		t.Fatalf("parse partial-tail media: %v", err)
	}
	var audio []*avframe.AVFrame
	for _, frame := range frames {
		if frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader {
			audio = append(audio, frame)
		}
	}
	if len(audio) != len(expected) {
		t.Fatalf("partial-tail fMP4 AAC frames = %d, want captured pipeline sequence length %d", len(audio), len(expected))
	}
	for i := range audio {
		if !bytes.Equal(audio[i].Payload, expected[i]) {
			t.Fatalf("partial-tail fMP4 AAC payload[%d] differs from captured pipeline output", i)
		}
		if i > 0 && audio[i].DTS <= audio[i-1].DTS {
			t.Fatalf("partial-tail fMP4 DTS[%d] = %d after %d, want strictly increasing", i, audio[i].DTS, audio[i-1].DTS)
		}
	}
}

func readRecordAACPayloadsUntilClose(t *testing.T, reader interface {
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
				t.Fatalf("captured recording transcode reader did not close: %v", ctx.Err())
			}
			break
		}
		if frame != nil && frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader {
			payloads = append(payloads, append([]byte(nil), frame.Payload...))
		}
	}
	if frame, ok := reader.TryRead(); ok {
		t.Fatalf("captured recording transcode reader returned extra frame after close: %+v", frame)
	}
	return payloads
}
