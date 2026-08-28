//go:build audiocodec

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
	"github.com/im-pingo/liveforge/pkg/audiocodec"
	"github.com/im-pingo/liveforge/pkg/avframe"
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
