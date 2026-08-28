package sipgateway

import (
	"strings"

	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/sdp"
)

type codecInfo struct {
	Codec     avframe.CodecType
	ClockRate int
	PT        int
}

var encodingToCodec = map[string]codecInfo{
	"PCMU": {avframe.CodecG711U, 8000, 0},
	"PCMA": {avframe.CodecG711A, 8000, 8},
	"G722": {avframe.CodecG722, 8000, 9},
	"OPUS": {avframe.CodecOpus, 48000, 111},
}

var sipH264Codec = negotiatedCodec{
	Codec:        avframe.CodecH264,
	PT:           96,
	ClockRate:    90000,
	EncodingName: "H264",
}

func codecForEncoding(name string) (codecInfo, bool) {
	info, ok := encodingToCodec[strings.ToUpper(name)]
	return info, ok
}

func configuredCodecForSource(configured []string, source avframe.CodecType) (negotiatedCodec, bool) {
	for _, name := range configured {
		info, ok := codecForEncoding(name)
		if !ok || info.Codec != source {
			continue
		}
		encodingName := strings.ToUpper(name)
		if info.Codec == avframe.CodecOpus {
			encodingName = "opus"
		}
		return negotiatedCodec{
			Codec:        info.Codec,
			PT:           info.PT,
			ClockRate:    info.ClockRate,
			EncodingName: encodingName,
		}, true
	}
	return negotiatedCodec{}, false
}

type negotiatedCodec struct {
	Codec        avframe.CodecType
	PT           int
	ClockRate    int
	EncodingName string
}

func negotiateCodec(offer *sdp.MediaDescription, preferred []string) (negotiatedCodec, bool) {
	priorityMap := make(map[string]int)
	for i, name := range preferred {
		priorityMap[strings.ToUpper(name)] = len(preferred) - i
	}

	var best negotiatedCodec
	bestPriority := -1

	for _, pt := range offer.Formats {
		rm := offer.RTPMap(pt)
		if rm == nil {
			if info, ok := staticPT(pt); ok {
				rm = &sdp.RTPMapInfo{
					PayloadType:  pt,
					EncodingName: info.EncodingName,
					ClockRate:    info.ClockRate,
				}
			} else {
				continue
			}
		}

		nameUpper := strings.ToUpper(rm.EncodingName)
		info, supported := codecForEncoding(rm.EncodingName)
		if !supported || rm.ClockRate != info.ClockRate {
			continue
		}

		prio, inPreferred := priorityMap[nameUpper]
		if !inPreferred {
			continue
		}

		if prio > bestPriority {
			bestPriority = prio
			best = negotiatedCodec{
				Codec:        info.Codec,
				PT:           pt,
				ClockRate:    rm.ClockRate,
				EncodingName: rm.EncodingName,
			}
		}
	}

	return best, bestPriority >= 0
}

func negotiateH264(offer *sdp.MediaDescription) (negotiatedCodec, bool) {
	if offer == nil || offer.Type != "video" {
		return negotiatedCodec{}, false
	}
	for _, pt := range offer.Formats {
		rm := offer.RTPMap(pt)
		if rm != nil && strings.EqualFold(rm.EncodingName, "H264") && rm.ClockRate == sipH264Codec.ClockRate {
			codec := sipH264Codec
			codec.PT = pt
			return codec, true
		}
	}
	return negotiatedCodec{}, false
}

func staticPT(pt int) (struct {
	EncodingName string
	ClockRate    int
}, bool) {
	type ptInfo struct {
		EncodingName string
		ClockRate    int
	}
	m := map[int]ptInfo{
		0:  {"PCMU", 8000},
		8:  {"PCMA", 8000},
		9:  {"G722", 8000},
		18: {"G729", 8000},
	}
	info, ok := m[pt]
	return info, ok
}

func buildAnswerSDP(serverAddr string, rtpPort int, nc negotiatedCodec) []byte {
	return buildAnswerSDPWithVideo(serverAddr, rtpPort, nc, 0, negotiatedCodec{})
}

func buildAnswerSDPWithVideo(serverAddr string, audioPort int, audioCodec negotiatedCodec, videoPort int, videoCodec negotiatedCodec) []byte {
	sd := &sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      "1",
			SessionVersion: "1",
			NetType:        "IN",
			AddrType:       "IP4",
			Address:        serverAddr,
		},
		Name: "LiveForge SIP Gateway",
		Connection: &sdp.Connection{
			NetType:  "IN",
			AddrType: "IP4",
			Address:  serverAddr,
		},
		Timing: sdp.Timing{Start: 0, Stop: 0},
	}

	md := &sdp.MediaDescription{
		Type:    "audio",
		Port:    audioPort,
		Proto:   "RTP/AVP",
		Formats: []int{audioCodec.PT},
		Attributes: []sdp.Attribute{
			{Key: "rtpmap", Value: rtpmapValue(audioCodec)},
			{Key: "sendrecv"},
			{Key: "ptime", Value: "20"},
		},
	}

	sd.Media = append(sd.Media, md)
	appendH264Media(sd, videoPort, videoCodec)
	return sd.Marshal()
}

func buildOfferSDP(serverAddr string, rtpPort int, codecs []negotiatedCodec) []byte {
	return buildOfferSDPWithVideo(serverAddr, rtpPort, codecs, 0, negotiatedCodec{})
}

func buildOfferSDPWithVideo(serverAddr string, audioPort int, codecs []negotiatedCodec, videoPort int, videoCodec negotiatedCodec) []byte {
	sd := &sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      "1",
			SessionVersion: "1",
			NetType:        "IN",
			AddrType:       "IP4",
			Address:        serverAddr,
		},
		Name: "LiveForge SIP Gateway",
		Connection: &sdp.Connection{
			NetType:  "IN",
			AddrType: "IP4",
			Address:  serverAddr,
		},
		Timing: sdp.Timing{Start: 0, Stop: 0},
	}

	var formats []int
	var attrs []sdp.Attribute
	for _, nc := range codecs {
		formats = append(formats, nc.PT)
		attrs = append(attrs, sdp.Attribute{Key: "rtpmap", Value: rtpmapValue(nc)})
	}
	attrs = append(attrs, sdp.Attribute{Key: "sendrecv"})
	attrs = append(attrs, sdp.Attribute{Key: "ptime", Value: "20"})

	md := &sdp.MediaDescription{
		Type:       "audio",
		Port:       audioPort,
		Proto:      "RTP/AVP",
		Formats:    formats,
		Attributes: attrs,
	}
	sd.Media = append(sd.Media, md)
	appendH264Media(sd, videoPort, videoCodec)
	return sd.Marshal()
}

func appendH264Media(sd *sdp.SessionDescription, port int, codec negotiatedCodec) {
	if sd == nil || port <= 0 || codec.Codec != avframe.CodecH264 {
		return
	}
	sd.Media = append(sd.Media, &sdp.MediaDescription{
		Type:    "video",
		Port:    port,
		Proto:   "RTP/AVP",
		Formats: []int{codec.PT},
		Attributes: []sdp.Attribute{
			{Key: "rtpmap", Value: rtpmapValue(codec)},
			{Key: "fmtp", Value: itoa(codec.PT) + " packetization-mode=1;profile-level-id=42c00b"},
			{Key: "sendrecv"},
		},
	})
}

func rtpmapValue(nc negotiatedCodec) string {
	if nc.Codec == avframe.CodecOpus {
		return strings.Join([]string{
			itoa(nc.PT), " ", nc.EncodingName, "/", itoa(nc.ClockRate), "/2",
		}, "")
	}
	return strings.Join([]string{
		itoa(nc.PT), " ", nc.EncodingName, "/", itoa(nc.ClockRate),
	}, "")
}

func itoa(n int) string {
	buf := [20]byte{}
	pos := len(buf)
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
