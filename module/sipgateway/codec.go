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
	"PCMU":           {avframe.CodecG711U, 8000, 0},
	"PCMA":           {avframe.CodecG711A, 8000, 8},
	"G722":           {avframe.CodecG722, 8000, 9},
	"G729":           {avframe.CodecG729, 8000, 18},
	"opus":           {avframe.CodecOpus, 48000, 111},
	"speex":          {avframe.CodecSpeex, 16000, 102},
	"MPEG4-GENERIC":  {avframe.CodecAAC, 44100, 101},
}

type negotiatedCodec struct {
	Codec       avframe.CodecType
	PT          int
	ClockRate   int
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
		info, supported := encodingToCodec[rm.EncodingName]
		if !supported {
			info, supported = encodingToCodec[nameUpper]
			if !supported {
				continue
			}
		}

		prio, inPreferred := priorityMap[nameUpper]
		if !inPreferred {
			prio = 0
		}

		if prio > bestPriority {
			bestPriority = prio
			best = negotiatedCodec{
				Codec:       info.Codec,
				PT:          pt,
				ClockRate:   rm.ClockRate,
				EncodingName: rm.EncodingName,
			}
		}
	}

	return best, bestPriority >= 0
}

func staticPT(pt int) (struct{ EncodingName string; ClockRate int }, bool) {
	type ptInfo struct{ EncodingName string; ClockRate int }
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
		Port:    rtpPort,
		Proto:   "RTP/AVP",
		Formats: []int{nc.PT},
		Attributes: []sdp.Attribute{
			{Key: "rtpmap", Value: rtpmapValue(nc)},
			{Key: "sendrecv"},
			{Key: "ptime", Value: "20"},
		},
	}

	sd.Media = append(sd.Media, md)
	return sd.Marshal()
}

func buildOfferSDP(serverAddr string, rtpPort int, codecs []negotiatedCodec) []byte {
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
		Port:       rtpPort,
		Proto:      "RTP/AVP",
		Formats:    formats,
		Attributes: attrs,
	}
	sd.Media = append(sd.Media, md)
	return sd.Marshal()
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
