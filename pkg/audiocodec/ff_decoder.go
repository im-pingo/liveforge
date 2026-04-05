package audiocodec

/*
#include <libavcodec/avcodec.h>
#include <libavutil/frame.h>
#include <libavutil/channel_layout.h>
#include <libavutil/mem.h>
#include <libavutil/opt.h>
#include <stdlib.h>
#include <string.h>

// ff_decoder_open locates the decoder by name, allocates a context,
// configures it for the given sample-rate / channel-count (needed for
// raw PCM codecs such as pcm_mulaw / pcm_alaw), and opens it.
static int ff_decoder_open(const char *name, int sample_rate, int channels,
                           AVCodecContext **out_ctx) {
    const AVCodec *codec = avcodec_find_decoder_by_name(name);
    if (!codec) return -1;

    AVCodecContext *ctx = avcodec_alloc_context3(codec);
    if (!ctx) return -2;

    // PCM codecs require explicit format parameters.
    ctx->sample_rate = sample_rate;
    av_channel_layout_default(&ctx->ch_layout, channels);
    // Many PCM codecs default to S16; set it explicitly just in case.
    if (ctx->codec->id == AV_CODEC_ID_PCM_MULAW ||
        ctx->codec->id == AV_CODEC_ID_PCM_ALAW) {
        ctx->sample_fmt = AV_SAMPLE_FMT_S16;
    }

    int ret = avcodec_open2(ctx, codec, NULL);
    if (ret < 0) {
        avcodec_free_context(&ctx);
        return ret;
    }
    *out_ctx = ctx;
    return 0;
}

// ff_decoder_set_extradata copies extradata into the codec context.
static int ff_decoder_set_extradata(AVCodecContext *ctx,
                                    const uint8_t *data, int size) {
    av_freep(&ctx->extradata);
    ctx->extradata_size = 0;
    if (size <= 0) return 0;
    ctx->extradata = (uint8_t *)av_mallocz(size + AV_INPUT_BUFFER_PADDING_SIZE);
    if (!ctx->extradata) return -1;
    memcpy(ctx->extradata, data, size);
    ctx->extradata_size = size;
    return 0;
}

// ff_decode sends a raw payload through the decoder and writes the
// resulting S16-interleaved samples into *out / *out_len.
// The caller must free *out with free().
static int ff_decode(AVCodecContext *ctx,
                     const uint8_t *payload, int payload_len,
                     int16_t **out, int *out_samples, int *out_channels,
                     int *out_sample_rate) {
    AVPacket *pkt = av_packet_alloc();
    if (!pkt) return -1;

    pkt->data = (uint8_t *)payload;
    pkt->size = payload_len;

    int ret = avcodec_send_packet(ctx, pkt);
    av_packet_free(&pkt);
    if (ret < 0) return ret;

    AVFrame *frame = av_frame_alloc();
    if (!frame) return -2;

    ret = avcodec_receive_frame(ctx, frame);
    if (ret < 0) {
        av_frame_free(&frame);
        return ret;
    }

    int nb_samples = frame->nb_samples;
    int channels   = frame->ch_layout.nb_channels;
    int total      = nb_samples * channels;

    int16_t *buf = (int16_t *)malloc(total * sizeof(int16_t));
    if (!buf) {
        av_frame_free(&frame);
        return -3;
    }

    enum AVSampleFormat fmt = (enum AVSampleFormat)frame->format;
    if (fmt == AV_SAMPLE_FMT_S16) {
        // Interleaved S16 — direct copy.
        memcpy(buf, frame->data[0], total * sizeof(int16_t));
    } else if (fmt == AV_SAMPLE_FMT_S16P) {
        // Planar S16 — interleave.
        for (int s = 0; s < nb_samples; s++) {
            for (int c = 0; c < channels; c++) {
                int16_t *plane = (int16_t *)frame->data[c];
                buf[s * channels + c] = plane[s];
            }
        }
    } else if (fmt == AV_SAMPLE_FMT_FLTP) {
        // Planar float — convert + interleave.
        for (int s = 0; s < nb_samples; s++) {
            for (int c = 0; c < channels; c++) {
                float *plane = (float *)frame->data[c];
                float v = plane[s];
                if (v > 1.0f)  v = 1.0f;
                if (v < -1.0f) v = -1.0f;
                buf[s * channels + c] = (int16_t)(v * 32767.0f);
            }
        }
    } else if (fmt == AV_SAMPLE_FMT_FLT) {
        // Interleaved float — convert.
        float *src = (float *)frame->data[0];
        for (int i = 0; i < total; i++) {
            float v = src[i];
            if (v > 1.0f)  v = 1.0f;
            if (v < -1.0f) v = -1.0f;
            buf[i] = (int16_t)(v * 32767.0f);
        }
    } else {
        free(buf);
        av_frame_free(&frame);
        return -4; // unsupported format
    }

    *out             = buf;
    *out_samples     = total;
    *out_channels    = channels;
    *out_sample_rate = frame->sample_rate;

    av_frame_free(&frame);
    return 0;
}
*/
import "C"

import (
	"fmt"
	"log/slog"
	"unsafe"
)

// FFmpegDecoder decodes compressed audio into PCM using FFmpeg's C API.
type FFmpegDecoder struct {
	ctx        *C.AVCodecContext
	codecName  string
	sampleRate int
	channels   int
}

// NewFFmpegDecoder creates a decoder for the given FFmpeg codec name
// (e.g. "pcm_mulaw", "pcm_alaw", "aac", "libopus").
func NewFFmpegDecoder(codecName string) *FFmpegDecoder {
	// Sensible defaults — PCM codecs need explicit params.
	sr := 8000
	ch := 1
	switch codecName {
	case "aac":
		sr = 44100
		ch = 2
	case "libopus":
		sr = 48000
		ch = 2
	case "mp3float":
		sr = 44100
		ch = 2
	case "g722":
		sr = 16000
		ch = 1
	case "libspeex":
		sr = 8000
		ch = 1
	}

	cName := C.CString(codecName)
	defer C.free(unsafe.Pointer(cName))

	var ctx *C.AVCodecContext
	ret := C.ff_decoder_open(cName, C.int(sr), C.int(ch), &ctx)
	if ret != 0 {
		slog.Warn("FFmpeg decoder open failed, returning inactive decoder",
			"codec", codecName, "sample_rate", sr, "channels", ch, "ret", int(ret))
		return &FFmpegDecoder{codecName: codecName, sampleRate: sr, channels: ch}
	}
	return &FFmpegDecoder{
		ctx:        ctx,
		codecName:  codecName,
		sampleRate: int(ctx.sample_rate),
		channels:   int(ctx.ch_layout.nb_channels),
	}
}

func (d *FFmpegDecoder) SetExtradata(data []byte) {
	if d.ctx == nil || len(data) == 0 {
		return
	}
	C.ff_decoder_set_extradata(d.ctx, (*C.uint8_t)(unsafe.Pointer(&data[0])), C.int(len(data)))
}

func (d *FFmpegDecoder) Decode(payload []byte) (*PCMFrame, error) {
	if d.ctx == nil {
		return nil, fmt.Errorf("ffmpeg decoder %q: context not initialised", d.codecName)
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("ffmpeg decoder %q: empty payload", d.codecName)
	}

	var (
		out        *C.int16_t
		outSamples C.int
		outCh      C.int
		outSR      C.int
	)

	ret := C.ff_decode(d.ctx,
		(*C.uint8_t)(unsafe.Pointer(&payload[0])), C.int(len(payload)),
		&out, &outSamples, &outCh, &outSR)
	if ret != 0 {
		return nil, fmt.Errorf("ffmpeg decoder %q: decode error %d", d.codecName, int(ret))
	}
	defer C.free(unsafe.Pointer(out))

	total := int(outSamples)
	samples := make([]int16, total)
	// Copy from C buffer into Go slice.
	src := unsafe.Slice((*int16)(unsafe.Pointer(out)), total)
	copy(samples, src)

	return &PCMFrame{
		Samples:    samples,
		SampleRate: int(outSR),
		Channels:   int(outCh),
	}, nil
}

func (d *FFmpegDecoder) SampleRate() int { return d.sampleRate }
func (d *FFmpegDecoder) Channels() int   { return d.channels }

func (d *FFmpegDecoder) Close() {
	if d.ctx != nil {
		C.avcodec_free_context(&d.ctx)
		d.ctx = nil
	}
}
