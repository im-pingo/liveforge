package rtp

import (
	"github.com/im-pingo/liveforge/pkg/avframe"
	"github.com/im-pingo/liveforge/pkg/codec/h265"
)

// VideoToAnnexB converts an internal H.264 or H.265 payload to Annex-B.
// Sequence headers are parsed according to the declared codec instead of
// guessing from configurationVersion, which is 1 for both AVC and HEVC.
func VideoToAnnexB(codec avframe.CodecType, data []byte, isSequenceHeader bool) []byte {
	switch codec {
	case avframe.CodecH264:
		return ToAnnexB(data, isSequenceHeader)
	case avframe.CodecH265:
		if !isSequenceHeader {
			return ToAnnexB(data, false)
		}
		vps, sps, pps, err := h265.ExtractVPSSPSPPSFromHVCRecord(data)
		if err != nil {
			return nil
		}
		return h265ParameterSetsAnnexB(vps, sps, pps)
	default:
		return nil
	}
}
