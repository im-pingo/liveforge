package labmedia

import (
	"encoding/base64"

	"github.com/im-pingo/liveforge/pkg/avframe"
)

const (
	VideoFrameDurationMs int64 = 40
	AudioFrameDurationMs int64 = 20
)

var videoAccessUnits = decodeVideoAccessUnits()

// VideoFrame returns a dependency-free H.264 frame from a looping 25 fps test
// pattern. The first frame of every one-second loop is an IDR frame.
func VideoFrame(dtsMs int64) *avframe.AVFrame {
	index := int(dtsMs / VideoFrameDurationMs)
	if index < 0 {
		index = -index
	}
	index %= len(videoAccessUnits)
	frameType := avframe.FrameTypeInterframe
	if index == 0 {
		frameType = avframe.FrameTypeKeyframe
	}
	return avframe.NewAVFrame(
		avframe.MediaTypeVideo,
		avframe.CodecH264,
		frameType,
		dtsMs,
		dtsMs,
		videoAccessUnits[index],
	)
}

// G711Frame returns 20 ms of an audible 200 Hz square wave.
func G711Frame(codec avframe.CodecType, dtsMs int64) *avframe.AVFrame {
	payload := make([]byte, 160)
	startSample := dtsMs * 8
	for i := range payload {
		sample := int16(8000)
		if (startSample+int64(i))%40 >= 20 {
			sample = -8000
		}
		switch codec {
		case avframe.CodecG711U:
			payload[i] = linearToMuLaw(sample)
		default:
			payload[i] = linearToALaw(sample)
		}
	}
	return avframe.NewAVFrame(
		avframe.MediaTypeAudio,
		codec,
		avframe.FrameTypeInterframe,
		dtsMs,
		dtsMs,
		payload,
	)
}

func decodeVideoAccessUnits() [][]byte {
	data, err := base64.StdEncoding.DecodeString(h264TestPatternBase64)
	if err != nil {
		panic(err)
	}
	var units [][]byte
	for offset := 0; offset < len(data); {
		start := nextAUD(data, offset)
		if start < 0 {
			break
		}
		end := nextAUD(data, start+5)
		if end < 0 {
			end = len(data)
		}
		units = append(units, data[start:end])
		offset = end
	}
	if len(units) != 25 {
		panic("labmedia: invalid H.264 test pattern")
	}
	return units
}

func nextAUD(data []byte, offset int) int {
	for i := offset; i+5 <= len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 && data[i+4]&0x1f == 9 {
			return i
		}
	}
	return -1
}

func linearToMuLaw(sample int16) byte {
	value := int(sample)
	sign := byte(0)
	if value < 0 {
		sign = 0x80
		value = -value
	}
	if value > 32635 {
		value = 32635
	}
	value += 0x84
	exponent := 7
	for mask := 0x4000; exponent > 0 && value&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := (value >> (exponent + 3)) & 0x0f
	return ^(sign | byte(exponent<<4) | byte(mantissa))
}

func linearToALaw(sample int16) byte {
	value := int(sample) >> 3
	mask := byte(0xd5)
	if value < 0 {
		mask = 0x55
		value = -value - 1
	}
	segment := 0
	for limit := 0x20; segment < 8 && value >= limit; limit <<= 1 {
		segment++
	}
	if segment >= 8 {
		return 0x7f ^ mask
	}
	encoded := byte(segment << 4)
	if segment < 2 {
		encoded |= byte((value >> 1) & 0x0f)
	} else {
		encoded |= byte((value >> segment) & 0x0f)
	}
	return encoded ^ mask
}

const h264TestPatternBase64 = "AAAAAQkQAAAAAWdCwAvaCjfkwEQAAAMABAAAAwDKPFCqgAAAAAFozgnIAAABBgX//1rcRem95tlIt5Ys2CDZI+7veDI2NCAtIGNvcmUgMTY0IHIzMTA4IDMxZTE5ZjkgLSBILjI2NC9NUEVHLTQgQVZDIGNvZGVjIC0gQ29weWxlZnQgMjAwMy0yMDIzIC0gaHR0cDovL3d3dy52aWRlb2xhbi5vcmcveDI2NC5odG1sIC0gb3B0aW9uczogY2FiYWM9MCByZWY9MSBkZWJsb2NrPTE6MDowIGFuYWx5c2U9MHgxOjB4MTExIG1lPWhleCBzdWJtZT0yIHBzeT0xIHBzeV9yZD0xLjAwOjAuMDAgbWl4ZWRfcmVmPTAgbWVfcmFuZ2U9MTYgY2hyb21hX21lPTEgdHJlbGxpcz0wIDh4OGRjdD0wIGNxbT0wIGRlYWR6b25lPTIxLDExIGZhc3RfcHNraXA9MSBjaHJvbWFfcXBfb2Zmc2V0PTAgdGhyZWFkcz0xIGxvb2thaGVhZF90aHJlYWRzPTEgc2xpY2VkX3RocmVhZHM9MCBucj0wIGRlY2ltYXRlPTEgaW50ZXJsYWNlZD0wIGJsdXJheV9jb21wYXQ9MCBjb25zdHJhaW5lZF9pbnRyYT0wIGJmcmFtZXM9MCB3ZWlnaHRwPTAga2V5aW50PTI1IGtleWludF9taW49MTMgc2NlbmVjdXQ9MCBpbnRyYV9yZWZyZXNoPTAgcmM9Y3JmIG1idHJlZT0wIGNyZj0yOC4wIHFjb21wPTAuNjAgcXBtaW49MCBxcG1heD02OSBxcHN0ZXA9NCBpcF9yYXRpbz0xLjQwIGFxPTE6MS4wMACAAAABZYiEBL///wRRQABDX8cAAQco4AAguycnJycnJycnJ11111//EPwSAsAcQxR0t8QAAQDgAdCACZNTZ/x4eEYQBOcwCKYLa66666+GAf+wRRQAPj4+1mYLKWmC2uuuuuunrp6euuuuuvj//oFYKAHCIQ2W+IACKhKQHBFJlhwRSZfgypZSpftXjw/+gVQhIyAJgcIhEvwauX0wU111111311114AAAAAEJMAAAAUGaIBLwl0ChkFcAAAABCTAAAAFBmkAU8IcWCo1DUzUZ4rDEEcAAAAABCTAAAAFBmmAV8JdAqcTwVz4QgjgAAAABCTAAAAFBmoAW8HsAAAABCTAAAAFBmqAW8IcoKtVBB0fUbAAAAAEJMAAAAUGawBbwewAAAAEJMAAAAUGa4BbwewAAAAEJMAAAAUGbABbwewAAAAEJMAAAAUGbIBbwewAAAAEJMAAAAUGbQBbwewAAAAEJMAAAAUGbYBbwewAAAAEJMAAAAUGbgBbwewAAAAEJMAAAAUGboBbwewAAAAEJMAAAAUGbwBbwewAAAAEJMAAAAUGb4BbwewAAAAEJMAAAAUGaABbwewAAAAEJMAAAAUGaIBbwewAAAAEJMAAAAUGaQBbwewAAAAEJMAAAAUGaYBbwewAAAAEJMAAAAUGagBbwewAAAAEJMAAAAUGaoBbwewAAAAEJMAAAAUGawBbwewAAAAEJMAAAAUGa4BbwewAAAAEJMAAAAUGbABbwew=="
