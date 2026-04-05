package audiocodec

// TsTracker maintains an independent audio timeline anchored to the first
// source frame's DTS. Timestamps are derived from cumulative encoder output
// sample counts, ensuring precise A/V sync without floating-point drift.
type TsTracker struct {
	baseTime       int64
	samplesEncoded int64
	sampleRate     int
}

// Init anchors the tracker to the first frame's DTS and target sample rate.
func (t *TsTracker) Init(firstFrameDTS int64, sampleRate int) {
	t.baseTime = firstFrameDTS
	t.sampleRate = sampleRate
	t.samplesEncoded = 0
}

// Next returns the DTS for the current frame and advances the sample counter.
func (t *TsTracker) Next(frameSamples int) int64 {
	dts := t.baseTime + (t.samplesEncoded*1000)/int64(t.sampleRate)
	t.samplesEncoded += int64(frameSamples)
	return dts
}
