package dvr

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/im-pingo/liveforge/config"
	"github.com/im-pingo/liveforge/core"
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/muxer/ts"
)

func TestSessionRotatesLiveAudioOnlyAtSegmentDuration(t *testing.T) {
	dir := t.TempDir()
	stream := core.NewStream("live/audio-only-dvr", config.StreamConfig{RingBufferSize: 256}, config.LimitsConfig{}, core.NewEventBus())
	defer stream.Close()
	if err := stream.SetPublisher(&dvrStaleHistoryPublisher{
		id:   "audio-only-publisher",
		info: &avframe.MediaInfo{AudioCodec: avframe.CodecAAC, SampleRate: 44100, Channels: 2},
	}); err != nil {
		t.Fatal(err)
	}
	stream.WriteFrame(avframe.NewAVFrame(
		avframe.MediaTypeAudio,
		avframe.CodecAAC,
		avframe.FrameTypeSequenceHeader,
		0,
		0,
		[]byte{0x12, 0x10},
	))

	session, err := NewSession("live/audio-only-dvr", stream, config.DVRConfig{
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
	for dts := int64(0); dts <= 1200; dts += 20 {
		stream.WriteFrame(avframe.NewAVFrame(
			avframe.MediaTypeAudio,
			avframe.CodecAAC,
			avframe.FrameTypeInterframe,
			dts,
			dts,
			[]byte{0x11, 0x22, 0x33, byte(dts / 20)},
		))
	}

	deadline := time.Now().Add(2 * time.Second)
	for session.Index().Len() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if session.Index().Len() < 1 {
		t.Fatal("live audio-only DVR did not finalize a segment before publisher stop")
	}
	first, ok := session.Index().First()
	if !ok || first.Duration <= 0 || first.Duration > 2 {
		t.Fatalf("audio-only first segment = %#v, want positive bounded duration", first)
	}
	if _, err := os.Stat(first.DiskPath); err != nil {
		t.Fatalf("audio-only segment is not present while publisher is live: %v", err)
	}

	session.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("audio-only DVR session did not stop")
	}

	audioFrames := 0
	for _, segment := range session.Index().Segments() {
		data, err := os.ReadFile(segment.DiskPath)
		if err != nil {
			t.Fatal(err)
		}
		demuxer := ts.NewDemuxer(func(frame *avframe.AVFrame) {
			if frame.MediaType.IsAudio() && frame.FrameType != avframe.FrameTypeSequenceHeader {
				audioFrames++
			}
			if frame.MediaType.IsVideo() {
				t.Errorf("audio-only DVR emitted video frame: %#v", frame)
			}
		})
		demuxer.Feed(data)
		demuxer.Flush()
	}
	if audioFrames == 0 {
		t.Fatal("audio-only DVR segments contained no demuxable audio frames")
	}
}
