package webrtc

import "encoding/binary"

// buildTestAVCConfigPayload creates a minimal AVCDecoderConfigurationRecord.
func buildTestAVCConfigPayload(sps, pps []byte) []byte {
	spsLen := uint16(len(sps)) //nolint:gosec // test SPS is always tiny
	ppsLen := uint16(len(pps)) //nolint:gosec // test PPS is always tiny
	buf := make([]byte, 0, 11+len(sps)+len(pps))
	buf = append(buf,
		0x01,   // configurationVersion
		sps[1], // AVCProfileIndication
		sps[2], // profile_compatibility
		sps[3], // AVCLevelIndication
		0xFF,   // lengthSizeMinusOne = 3 (4-byte NALU length)
		0xE1,   // numOfSequenceParameterSets = 1
	)
	buf = binary.BigEndian.AppendUint16(buf, spsLen)
	buf = append(buf, sps...)
	buf = append(buf, 0x01) // numOfPictureParameterSets = 1
	buf = binary.BigEndian.AppendUint16(buf, ppsLen)
	buf = append(buf, pps...)
	return buf
}
