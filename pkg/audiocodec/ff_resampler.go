package audiocodec

/*
#include <libswresample/swresample.h>
#include <libavutil/channel_layout.h>
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
// It feeds the input then flushes buffered samples so the full expected
// output is returned in a single call.
// Returns the total number of output samples per channel, or negative on error.
// The caller must free *out with free().
static int ff_resample(SwrContext *ctx,
                       const int16_t *in, int in_count,
                       int out_channels, int in_rate, int out_rate,
                       int16_t **out) {
    // Upper bound: input samples scaled by rate ratio, plus any delay
    // already buffered, plus padding.
    int64_t delay   = swr_get_delay(ctx, (int64_t)in_rate);
    int64_t out_max = (delay + (int64_t)in_count) * (int64_t)out_rate / (int64_t)in_rate + 32;

    int16_t *buf = (int16_t *)malloc((size_t)(out_max * out_channels) * sizeof(int16_t));
    if (!buf) return -1;

    const uint8_t *in_data[1]  = { (const uint8_t *)in };
    uint8_t       *out_data[1] = { (uint8_t *)buf };

    // Feed input data.
    int got = swr_convert(ctx, out_data, (int)out_max, in_data, in_count);
    if (got < 0) {
        free(buf);
        return got;
    }

    int total = got;

    // Flush any remaining buffered samples.
    while (swr_get_delay(ctx, (int64_t)out_rate) > 0) {
        uint8_t *flush_data[1] = { (uint8_t *)(buf + total * out_channels) };
        int flushed = swr_convert(ctx, flush_data, (int)(out_max - total), NULL, 0);
        if (flushed <= 0) break;
        total += flushed;
    }

    *out = buf;
    return total;
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
	if r.ctx == nil || len(pcm.Samples) == 0 {
		return &PCMFrame{SampleRate: r.outRate, Channels: r.outChannels}
	}

	inCount := len(pcm.Samples) / pcm.Channels

	var out *C.int16_t
	ret := C.ff_resample(r.ctx,
		(*C.int16_t)(unsafe.Pointer(&pcm.Samples[0])),
		C.int(inCount),
		C.int(r.outChannels),
		C.int(r.inRate),
		C.int(r.outRate),
		&out)
	if ret < 0 {
		return &PCMFrame{SampleRate: r.outRate, Channels: r.outChannels}
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
	}
}

// Close releases the underlying SwrContext.
func (r *FFmpegResampler) Close() {
	if r.ctx != nil {
		C.swr_free(&r.ctx)
		r.ctx = nil
	}
}
