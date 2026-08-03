package webrtc

// buildTestAVCConfigPayload creates a minimal AVCDecoderConfigurationRecord.
func buildTestAVCConfigPayload(sps, pps []byte) []byte {
	buf := []byte{
		0x01,       // configurationVersion
		sps[1],     // AVCProfileIndication
		sps[2],     // profile_compatibility
		sps[3],     // AVCLevelIndication
		0xFF,       // lengthSizeMinusOne = 3 (4-byte NALU length)
		0xE1,       // numOfSequenceParameterSets = 1
		byte(len(sps) >> 8), byte(len(sps)), // SPS length
	}
	buf = append(buf, sps...)
	buf = append(buf, 0x01) // numOfPictureParameterSets = 1
	buf = append(buf, byte(len(pps)>>8), byte(len(pps)))
	buf = append(buf, pps...)
	return buf
}
