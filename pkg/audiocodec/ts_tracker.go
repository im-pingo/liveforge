package audiocodec

// tsTracker maintains an independent audio timeline anchored to the first
// source frame's DTS. Timestamps are derived from cumulative encoder output
// sample counts, ensuring precise A/V sync without floating-point drift.
type tsTracker struct {
	baseTime       int64
	samplesEncoded int64
	sampleRate     int
}

func (t *tsTracker) init(firstFrameDTS int64, sampleRate int) {
	t.baseTime = firstFrameDTS
	t.sampleRate = sampleRate
	t.samplesEncoded = 0
}

func (t *tsTracker) next(frameSamples int) int64 {
	dts := t.baseTime + (t.samplesEncoded*1000)/int64(t.sampleRate)
	t.samplesEncoded += int64(frameSamples)
	return dts
}
