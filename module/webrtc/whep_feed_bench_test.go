package webrtc

import "testing"

func BenchmarkWHEPFeedStatusRecordMedia(b *testing.B) {
	status := newWHEPFeedStatus(1, 1, "live")
	status.setExpectedMedia(true, false)
	status.RecordVideo(true)
	baseline := status.Snapshot().VideoFrames

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		status.RecordVideo(true)
	}
	b.StopTimer()

	snapshot := status.Snapshot()
	if snapshot.VideoFrames != baseline+uint64(b.N) { // #nosec G115 -- benchmark iteration count is bounded by testing.B.
		b.Fatalf("video frames = %d, want %d", snapshot.VideoFrames, baseline+uint64(b.N)) // #nosec G115 -- benchmark iteration count is bounded by testing.B.
	}
	if snapshot.State != WHEPFeedPlaying || !snapshot.ExpectedVideo || snapshot.ExpectedAudio {
		b.Fatalf("feed state = %+v, want playing video-only status", snapshot)
	}
	if snapshot.LastVideoAt.IsZero() || snapshot.UpdatedAt.IsZero() {
		b.Fatalf("media timestamps were not recorded: %+v", snapshot)
	}
}
