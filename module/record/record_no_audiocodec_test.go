package record

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/internal/labmedia"
	"github.com/im-pingo/liveforge/pkg/avframe"
)

func TestFMP4RecordSessionDropsUnsupportedAudioWithoutTranscoder(t *testing.T) {
	dir := t.TempDir()
	stream := core.NewStream("sip/record-g711-no-codec", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 256,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	if err := stream.SetPublisher(&testPublisher{
		id: "sip-record-g711-no-codec",
		info: &avframe.MediaInfo{
			VideoCodec: avframe.CodecH264,
			AudioCodec: avframe.CodecG711A,
			SampleRate: 8000,
			Channels:   1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH264,
		avframe.FrameTypeSequenceHeader,
		0,
		0,
		[]byte{0x01, 0x64, 0x00, 0x28, 0xff, 0xe1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28, 0x01, 0x00, 0x04, 0x68, 0xee, 0x3c, 0x80},
	))

	session, err := NewRecordSession("sip/record-g711-no-codec", stream, config.RecordConfig{
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
	for dts := int64(0); dts < 1200; dts += labmedia.VideoFrameDurationMs {
		stream.WriteFrame(labmedia.VideoFrame(dts))
		stream.WriteFrame(labmedia.G711Frame(avframe.CodecG711A, dts))
	}
	time.Sleep(150 * time.Millisecond)
	session.Stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("video-only fMP4 recording session did not stop")
	}

	if status := session.Status(); status.State != RecordingCompleted {
		t.Fatalf("video-only fMP4 recording status = %+v, want completed", status)
	}
	data, err := os.ReadFile(session.writer.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("avc1")) {
		t.Fatal("video-only fMP4 recording did not contain an H.264 track")
	}
	if bytes.Contains(data, []byte("mp4a")) {
		t.Fatal("video-only fMP4 recording unexpectedly declared an audio track")
	}
}

func TestGenerationManagedFMP4RecordHandlesHeaderlessNonAACAudio(t *testing.T) {
	dir := t.TempDir()
	videoSequenceHeader := []byte{
		0x01, 0x64, 0x00, 0x28,
		0xff, 0xe1, 0x00, 0x04, 0x67, 0x64, 0x00, 0x28,
		0x01, 0x00, 0x04, 0x68, 0xee, 0x3c, 0x80,
	}
	stream := core.NewStream("live/record-headerless-opus", config.StreamConfig{
		GOPCache:       true,
		GOPCacheNum:    1,
		RingBufferSize: 64,
	}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	if err := stream.SetPublisher(&testPublisher{
		id: "headerless-opus",
		info: &avframe.MediaInfo{
			VideoCodec:          avframe.CodecH264,
			VideoSequenceHeader: videoSequenceHeader,
			AudioCodec:          avframe.CodecOpus,
			SampleRate:          48000,
			Channels:            2,
		},
	}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH264,
		avframe.FrameTypeKeyframe,
		0,
		0,
		[]byte{0, 0, 0, 2, 0x65, 0x01},
	))
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio,
		avframe.CodecOpus,
		avframe.FrameTypeInterframe,
		0,
		0,
		[]byte{0xf8, 0xff},
	))

	session, err := NewRecordSession("live/record-headerless-opus", stream, config.RecordConfig{
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
	session.Stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("headerless Opus fMP4 recording session did not stop")
	}

	if status := session.Status(); status.State != RecordingCompleted {
		t.Fatalf("headerless Opus fMP4 recording status = %+v, want completed", status)
	}
	data, err := os.ReadFile(session.writer.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("avc1")) {
		t.Fatal("headerless Opus fMP4 recording did not contain an H.264 track")
	}
	if bytes.Contains(data, []byte("Opus")) || bytes.Contains(data, []byte("mp4a")) {
		t.Fatal("headerless Opus fMP4 recording declared unsupported direct audio")
	}
}
