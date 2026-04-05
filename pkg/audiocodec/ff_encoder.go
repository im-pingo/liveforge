package audiocodec

/*
#include <libavcodec/avcodec.h>
#include <libavutil/frame.h>
#include <libavutil/channel_layout.h>
#include <libavutil/mem.h>
#include <libavutil/opt.h>
#include <stdlib.h>
#include <string.h>

// ff_encoder_open locates the encoder by name, configures and opens it.
static int ff_encoder_open(const char *name, int sample_rate, int channels,
                           AVCodecContext **out_ctx) {
    const AVCodec *codec = avcodec_find_encoder_by_name(name);
    if (!codec) return -1;

    AVCodecContext *ctx = avcodec_alloc_context3(codec);
    if (!ctx) return -2;

    ctx->sample_rate = sample_rate;
    av_channel_layout_default(&ctx->ch_layout, channels);

    // Select sample format based on codec type.
    if (codec->id == AV_CODEC_ID_PCM_MULAW ||
        codec->id == AV_CODEC_ID_PCM_ALAW) {
        ctx->sample_fmt = AV_SAMPLE_FMT_S16;
    } else if (codec->id == AV_CODEC_ID_AAC) {
        ctx->sample_fmt = AV_SAMPLE_FMT_FLTP;
        ctx->bit_rate   = 128000;
    } else if (codec->id == AV_CODEC_ID_OPUS) {
        ctx->sample_fmt = AV_SAMPLE_FMT_S16;
        ctx->bit_rate   = 64000;
    } else if (codec->id == AV_CODEC_ID_MP3) {
        ctx->sample_fmt = AV_SAMPLE_FMT_S16P;
        ctx->bit_rate   = 128000;
    } else {
        // Default to S16 for unknown codecs.
        ctx->sample_fmt = AV_SAMPLE_FMT_S16;
    }

    // Allow experimental codecs (e.g. native AAC encoder).
    ctx->strict_std_compliance = FF_COMPLIANCE_EXPERIMENTAL;

    int ret = avcodec_open2(ctx, codec, NULL);
    if (ret < 0) {
        avcodec_free_context(&ctx);
        return ret;
    }

    // For codecs with fixed frame sizes (AAC, Opus), send a silent
    // priming frame so subsequent Encode calls produce immediate output.
    if (ctx->frame_size > 0) {
        AVFrame *primer = av_frame_alloc();
        if (!primer) {
            avcodec_free_context(&ctx);
            return -3;
        }
        primer->nb_samples  = ctx->frame_size;
        primer->format      = ctx->sample_fmt;
        av_channel_layout_copy(&primer->ch_layout, &ctx->ch_layout);
        primer->sample_rate = ctx->sample_rate;
        ret = av_frame_get_buffer(primer, 0);
        if (ret < 0) {
            av_frame_free(&primer);
            avcodec_free_context(&ctx);
            return ret;
        }
        // Zero-fill all planes for silence.
        for (int i = 0; i < AV_NUM_DATA_POINTERS && primer->buf[i]; i++) {
            memset(primer->data[i], 0, primer->buf[i]->size);
        }
        avcodec_send_frame(ctx, primer);
        av_frame_free(&primer);
        // Discard the priming packet (if any is produced already).
        AVPacket *discard = av_packet_alloc();
        if (discard) {
            avcodec_receive_packet(ctx, discard); // ignore result
            av_packet_free(&discard);
        }
    }

    *out_ctx = ctx;
    return 0;
}

// ff_encode encodes interleaved S16 samples into the codec's format.
// The caller must free *out with free().
static int ff_encode(AVCodecContext *ctx,
                     const int16_t *samples, int nb_samples, int channels,
                     uint8_t **out, int *out_size) {
    AVFrame *frame = av_frame_alloc();
    if (!frame) return -1;

    frame->nb_samples     = nb_samples;
    frame->format         = ctx->sample_fmt;
    av_channel_layout_copy(&frame->ch_layout, &ctx->ch_layout);
    frame->sample_rate    = ctx->sample_rate;

    int ret = av_frame_get_buffer(frame, 0);
    if (ret < 0) {
        av_frame_free(&frame);
        return ret;
    }

    // Fill frame data, converting from interleaved S16 if needed.
    enum AVSampleFormat fmt = ctx->sample_fmt;
    if (fmt == AV_SAMPLE_FMT_S16) {
        memcpy(frame->data[0], samples, nb_samples * channels * sizeof(int16_t));
    } else if (fmt == AV_SAMPLE_FMT_S16P) {
        for (int s = 0; s < nb_samples; s++) {
            for (int c = 0; c < channels; c++) {
                int16_t *plane = (int16_t *)frame->data[c];
                plane[s] = samples[s * channels + c];
            }
        }
    } else if (fmt == AV_SAMPLE_FMT_FLTP) {
        for (int s = 0; s < nb_samples; s++) {
            for (int c = 0; c < channels; c++) {
                float *plane = (float *)frame->data[c];
                plane[s] = (float)samples[s * channels + c] / 32768.0f;
            }
        }
    } else if (fmt == AV_SAMPLE_FMT_FLT) {
        float *dst = (float *)frame->data[0];
        for (int i = 0; i < nb_samples * channels; i++) {
            dst[i] = (float)samples[i] / 32768.0f;
        }
    } else {
        av_frame_free(&frame);
        return -4; // unsupported format
    }

    ret = avcodec_send_frame(ctx, frame);
    av_frame_free(&frame);
    if (ret < 0) return ret;

    AVPacket *pkt = av_packet_alloc();
    if (!pkt) return -2;

    ret = avcodec_receive_packet(ctx, pkt);
    if (ret < 0) {
        av_packet_free(&pkt);
        return ret;
    }

    // Copy packet data to caller-owned buffer.
    uint8_t *buf = (uint8_t *)malloc(pkt->size);
    if (!buf) {
        av_packet_free(&pkt);
        return -3;
    }
    memcpy(buf, pkt->data, pkt->size);
    *out      = buf;
    *out_size = pkt->size;

    av_packet_free(&pkt);
    return 0;
}

static int ff_encoder_frame_size(AVCodecContext *ctx) {
    return ctx->frame_size;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// FFmpegEncoder encodes PCM into compressed audio using FFmpeg's C API.
type FFmpegEncoder struct {
	ctx        *C.AVCodecContext
	codecName  string
	sampleRate int
	channels   int
}

// NewFFmpegEncoder creates an encoder for the given FFmpeg codec name
// (e.g. "pcm_mulaw", "pcm_alaw", "aac", "libopus").
func NewFFmpegEncoder(codecName string, sampleRate, channels int) *FFmpegEncoder {
	cName := C.CString(codecName)
	defer C.free(unsafe.Pointer(cName))

	var ctx *C.AVCodecContext
	ret := C.ff_encoder_open(cName, C.int(sampleRate), C.int(channels), &ctx)
	if ret != 0 {
		return &FFmpegEncoder{codecName: codecName, sampleRate: sampleRate, channels: channels}
	}
	return &FFmpegEncoder{
		ctx:        ctx,
		codecName:  codecName,
		sampleRate: int(ctx.sample_rate),
		channels:   int(ctx.ch_layout.nb_channels),
	}
}

func (e *FFmpegEncoder) Encode(pcm *PCMFrame) ([]byte, error) {
	if e.ctx == nil {
		return nil, fmt.Errorf("ffmpeg encoder %q: context not initialised", e.codecName)
	}
	if len(pcm.Samples) == 0 {
		return nil, fmt.Errorf("ffmpeg encoder %q: empty PCM frame", e.codecName)
	}

	nbSamples := len(pcm.Samples) / pcm.Channels

	var (
		out     *C.uint8_t
		outSize C.int
	)

	ret := C.ff_encode(e.ctx,
		(*C.int16_t)(unsafe.Pointer(&pcm.Samples[0])),
		C.int(nbSamples), C.int(pcm.Channels),
		&out, &outSize)
	if ret != 0 {
		return nil, fmt.Errorf("ffmpeg encoder %q: encode error %d", e.codecName, int(ret))
	}
	defer C.free(unsafe.Pointer(out))

	result := make([]byte, int(outSize))
	copy(result, unsafe.Slice((*byte)(unsafe.Pointer(out)), int(outSize)))
	return result, nil
}

func (e *FFmpegEncoder) SampleRate() int { return e.sampleRate }
func (e *FFmpegEncoder) Channels() int   { return e.channels }

func (e *FFmpegEncoder) FrameSize() int {
	if e.ctx == nil {
		return 0
	}
	return int(C.ff_encoder_frame_size(e.ctx))
}

func (e *FFmpegEncoder) Close() {
	if e.ctx != nil {
		C.avcodec_free_context(&e.ctx)
		e.ctx = nil
	}
}
