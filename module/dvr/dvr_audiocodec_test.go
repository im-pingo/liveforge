//go:build audiocodec

package dvr

import (
	"os"
	"path/filepath"
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
