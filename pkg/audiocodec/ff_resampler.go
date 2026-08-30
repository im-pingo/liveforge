//go:build audiocodec

package audiocodec

/*
#include <libswresample/swresample.h>
#include <libavutil/channel_layout.h>
#include <libavutil/mathematics.h>
#include <libavutil/opt.h>
#include <stdlib.h>

// ff_resampler_open allocates and initialises a SwrContext for the given
// input/output sample-rate and channel-count combination.
// All formats are S16 interleaved.
static int ff_resampler_open(int in_rate, int in_channels,
                             int out_rate, int out_channels,
                             SwrContext **out_ctx) {
    AVChannelLayout in_layout, out_layout;
    av_channel_layout_default(&in_layout, in_channels);
    av_channel_layout_default(&out_layout, out_channels);

    SwrContext *ctx = NULL;
    int ret = swr_alloc_set_opts2(&ctx,
        &out_layout, AV_SAMPLE_FMT_S16, out_rate,
        &in_layout,  AV_SAMPLE_FMT_S16, in_rate,
        0, NULL);
    if (ret < 0 || !ctx) return -1;

    ret = swr_init(ctx);
    if (ret < 0) {
        swr_free(&ctx);
        return ret;
    }

    *out_ctx = ctx;
    return 0;
}

// ff_resample converts interleaved S16 samples through the resampler.
// This is a streaming call: the resampler maintains internal state between
// invocations, so fractional sample positions are preserved across calls.
// Do NOT flush (swr_convert with NULL input) between calls — flushing
// resets the internal phase and causes cumulative sample-count drift that
// manifests as gradual audio slowdown in real-time playback.
// Returns the number of output samples per channel, or negative on error.
// The caller must free *out with free().
static int ff_resample(SwrContext *ctx,
                       const int16_t *in, int in_count,
                       int out_channels, int in_rate, int out_rate,
                       int16_t **out,
                       int64_t *retained_delay, int64_t *delay_base) {
    // Upper bound: input samples scaled by rate ratio, plus any delay
    // already buffered, plus padding.
    int64_t delay   = swr_get_delay(ctx, (int64_t)in_rate);
    int64_t out_max = (delay + (int64_t)in_count) * (int64_t)out_rate / (int64_t)in_rate + 32;

    int16_t *buf = (int16_t *)malloc((size_t)(out_max * out_channels) * sizeof(int16_t));
    if (!buf) return -1;

    const uint8_t *in_data[1]  = { (const uint8_t *)in };
    uint8_t       *out_data[1] = { (uint8_t *)buf };

    int got = swr_convert(ctx, out_data, (int)out_max, in_data, in_count);
    if (got < 0) {
        free(buf);
        return got;
    }

    *out = buf;
    int64_t gcd = av_gcd((int64_t)in_rate, (int64_t)out_rate);
    if (gcd <= 0) {
        free(buf);
        *out = NULL;
        return -1;
    }
    int64_t exact_base = ((int64_t)in_rate / gcd) * (int64_t)out_rate;
    *delay_base = exact_base;
    *retained_delay = swr_get_delay(ctx, exact_base);
    return got;
}

// ff_resampler_drain returns samples retained by the resampling filter after
// finite input ends. The caller repeats this until zero and frees *out.
static int ff_resampler_drain(SwrContext *ctx,
                              int out_channels, int out_rate,
                              int16_t **out) {
    int64_t delay = swr_get_delay(ctx, (int64_t)out_rate);
    int64_t out_max = delay + 32;
    if (out_max < 32) out_max = 32;

    int16_t *buf = (int16_t *)malloc((size_t)(out_max * out_channels) * sizeof(int16_t));
    if (!buf) return -1;

    uint8_t *out_data[1] = { (uint8_t *)buf };
    int got = swr_convert(ctx, out_data, (int)out_max, NULL, 0);
    if (got <= 0) {
        free(buf);
        return got;
    }

    *out = buf;
    return got;
}
*/
import "C"

import (
	"unsafe"
)

// FFmpegResampler converts PCM between different sample-rates and
// channel counts using FFmpeg's libswresample.
// Instances are NOT safe for concurrent use.
type FFmpegResampler struct {
	ctx         *C.SwrContext
	inRate      int
	outRate     int
	outChannels int
	drained     bool
	sourceSpans sourceSpanQueue
}

// NewFFmpegResampler creates a resampler that converts from
// (inRate, inChannels) to (outRate, outChannels).
func NewFFmpegResampler(inRate, inChannels, outRate, outChannels int) *FFmpegResampler {
	var ctx *C.SwrContext
	ret := C.ff_resampler_open(
		C.int(inRate), C.int(inChannels),
		C.int(outRate), C.int(outChannels),
		&ctx)
	if ret != 0 {
		return &FFmpegResampler{inRate: inRate, outRate: outRate, outChannels: outChannels}
	}
	return &FFmpegResampler{
		ctx:         ctx,
		inRate:      inRate,
		outRate:     outRate,
		outChannels: outChannels,
	}
}

// Resample converts pcm to the target sample-rate and channel layout.
// Returns a new PCMFrame; the input is not modified.
func (r *FFmpegResampler) Resample(pcm *PCMFrame) *PCMFrame {
	frame, _ := r.resample(pcm)
	return frame
}

// ResampleAttributed converts PCM and attributes output to every queued input
// span that may have contributed before measured retained-delay aging.
func (r *FFmpegResampler) ResampleAttributed(pcm *PCMFrame, sourceSpan SourceSpan) (*AttributedPCMFrame, error) {
	if !sourceSpan.Valid() {
		return nil, ErrInvalidSourceSpan
	}
	if r.ctx == nil || r.drained || len(pcm.Samples) == 0 {
		return attributePCMFrame(&PCMFrame{SampleRate: r.outRate, Channels: r.outChannels}, SourceSpan{})
	}

	inCount := len(pcm.Samples) / pcm.Channels
	r.sourceSpans.append(int64(inCount), sourceSpan)
	contributors := r.sourceSpans.span()
	frame, retainedInputSamples := r.resample(pcm)
	if retainedInputSamples >= 0 {
		r.sourceSpans.retainTail(retainedInputSamples)
	}
	if len(frame.Samples) == 0 {
		return attributePCMFrame(frame, SourceSpan{})
	}
	return attributePCMFrame(frame, contributors)
}

func (r *FFmpegResampler) resample(pcm *PCMFrame) (*PCMFrame, int64) {
	if r.ctx == nil || r.drained || len(pcm.Samples) == 0 {
		return &PCMFrame{SampleRate: r.outRate, Channels: r.outChannels}, 0
	}

	inCount := len(pcm.Samples) / pcm.Channels

	var (
		out           *C.int16_t
		retainedDelay C.int64_t
		delayBase     C.int64_t
	)
	ret := C.ff_resample(r.ctx,
		(*C.int16_t)(unsafe.Pointer(&pcm.Samples[0])),
		C.int(inCount),
		C.int(r.outChannels),
		C.int(r.inRate),
		C.int(r.outRate),
		&out,
		&retainedDelay,
		&delayBase)
	if ret < 0 {
		return &PCMFrame{SampleRate: r.outRate, Channels: r.outChannels}, -1
	}
	defer C.free(unsafe.Pointer(out))

	total := int(ret) * r.outChannels
	samples := make([]int16, total)
	src := unsafe.Slice((*int16)(unsafe.Pointer(out)), total)
	copy(samples, src)

	return &PCMFrame{
		Samples:    samples,
		SampleRate: r.outRate,
		Channels:   r.outChannels,
	}, ceilRetainedInputSamples(int64(retainedDelay), int64(delayBase), r.inRate)
}

// Drain flushes all samples retained by the resampling filter and returns
// them in Go-owned memory. Subsequent calls return an empty frame.
func (r *FFmpegResampler) Drain() *PCMFrame {
	result := r.drain()
	r.sourceSpans.clear()
	return result
}

// DrainAttributed flushes retained samples with the union of their remaining
// source contributors. Repeated calls return an empty frame with invalid span.
func (r *FFmpegResampler) DrainAttributed() (*AttributedPCMFrame, error) {
	contributors := r.sourceSpans.span()
	result := r.drain()
	r.sourceSpans.clear()
	return attributePCMFrame(result, contributors)
}

func (r *FFmpegResampler) drain() *PCMFrame {
	result := &PCMFrame{SampleRate: r.outRate, Channels: r.outChannels}
	if r.ctx == nil || r.drained {
		return result
	}
	r.drained = true

	for {
		var out *C.int16_t
		ret := C.ff_resampler_drain(
			r.ctx,
			C.int(r.outChannels),
			C.int(r.outRate),
			&out,
		)
		if ret <= 0 {
			return result
		}

		total := int(ret) * r.outChannels
		start := len(result.Samples)
		result.Samples = append(result.Samples, make([]int16, total)...)
		copy(result.Samples[start:], unsafe.Slice((*int16)(unsafe.Pointer(out)), total))
		C.free(unsafe.Pointer(out))
	}
}

// Close releases the underlying SwrContext.
func (r *FFmpegResampler) Close() {
	if r.ctx != nil {
		C.swr_free(&r.ctx)
		r.ctx = nil
	}
	r.sourceSpans.clear()
}
