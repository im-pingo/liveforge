package aac

import (
	"errors"
	"fmt"
)

// AACInfo holds parsed AAC AudioSpecificConfig information.
type AACInfo struct {
	ObjectType int
	SampleRate int
	Channels   int
}

// Standard AAC sample rate table (ISO 14496-3).
var sampleRates = [...]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
	16000, 12000, 11025, 8000, 7350,
}

// ParseAudioSpecificConfig parses an AAC AudioSpecificConfig and extracts codec info.
func ParseAudioSpecificConfig(data []byte) (*AACInfo, error) {
	if len(data) < 2 {
		return nil, errors.New("AudioSpecificConfig too short")
	}

	// audioObjectType (5 bits)
	objectType := int(data[0]>>3) & 0x1F
	if objectType == 31 {
		// Extended object type — need at least 3 bytes
		if len(data) < 3 {
			return nil, errors.New("AudioSpecificConfig too short for extended object type")
		}
		objectType = 32 + int(data[0]&0x07)<<3 | int(data[1]>>5)
	}

	// frequencyIndex (4 bits)
	freqIndex := int(data[0]&0x07)<<1 | int(data[1]>>7)
	sampleRate := 0
	if freqIndex == 0x0F {
		// 24-bit explicit sample rate follows
		if len(data) < 5 {
			return nil, errors.New("AudioSpecificConfig too short for explicit sample rate")
		}
		sampleRate = int(data[1]&0x7F)<<17 | int(data[2])<<9 | int(data[3])<<1 | int(data[4]>>7)
	} else if freqIndex < len(sampleRates) {
		sampleRate = sampleRates[freqIndex]
	} else {
		return nil, fmt.Errorf("invalid frequency index: %d", freqIndex)
	}

	// channelConfiguration (4 bits)
	channels := int(data[1]>>3) & 0x0F

	return &AACInfo{
		ObjectType: objectType,
		SampleRate: sampleRate,
		Channels:   channels,
	}, nil
}

// SampleRateIndex returns the MPEG-4 frequency index for a sample rate.
func SampleRateIndex(rate int) int {
	for i, r := range sampleRates {
		if r == rate {
			return i
		}
	}
	return 0x0F
}

// ParseADTSHeader parses an ADTS header and returns the AACInfo and header length.
// The header is either 7 bytes (no CRC) or 9 bytes (with CRC).
func ParseADTSHeader(data []byte) (*AACInfo, int, error) {
	if len(data) < 7 {
		return nil, 0, errors.New("ADTS header too short")
	}
	// Sync word: 0xFFF
	if data[0] != 0xFF || (data[1]&0xF0) != 0xF0 {
		return nil, 0, errors.New("invalid ADTS sync word")
	}

	hasCRC := (data[1] & 0x01) == 0 // protection_absent=0 means CRC present
	headerLen := 7
	if hasCRC {
		headerLen = 9
	}
	if len(data) < headerLen {
		return nil, 0, errors.New("ADTS header too short for CRC")
	}

	profile := int((data[2]>>6)&0x03) + 1 // ADTS profile is objectType-1
	freqIdx := int((data[2] >> 2) & 0x0F)
	channels := int((data[2]&0x01)<<2) | int((data[3]>>6)&0x03)

	sampleRate := 0
	if freqIdx < len(sampleRates) {
		sampleRate = sampleRates[freqIdx]
	} else {
		return nil, 0, fmt.Errorf("invalid ADTS frequency index: %d", freqIdx)
	}

	return &AACInfo{
		ObjectType: profile,
		SampleRate: sampleRate,
		Channels:   channels,
	}, headerLen, nil
}

// BuildAudioSpecificConfig builds a 2-byte AudioSpecificConfig from AACInfo.
func BuildAudioSpecificConfig(info *AACInfo) []byte {
	freqIdx := SampleRateIndex(info.SampleRate)
	// audioObjectType(5) + frequencyIndex(4) + channelConfiguration(4) + padding(3)
	b0 := byte(info.ObjectType<<3) | byte((freqIdx>>1)&0x07)
	b1 := byte((freqIdx&0x01)<<7) | byte((info.Channels&0x0F)<<3)
	return []byte{b0, b1}
}

// BuildADTSHeader builds a 7-byte ADTS header for an AAC frame.
func BuildADTSHeader(info *AACInfo, frameLength int) []byte {
	header := make([]byte, 7)
	totalLen := 7 + frameLength
	freqIdx := SampleRateIndex(info.SampleRate)
	profile := info.ObjectType - 1

	header[0] = 0xFF
	header[1] = 0xF1 // sync + MPEG-4, Layer 0, no CRC
	header[2] = byte((profile<<6)&0xC0) | byte((freqIdx<<2)&0x3C) | byte((info.Channels>>2)&0x01)
	header[3] = byte((info.Channels<<6)&0xC0) | byte((totalLen>>11)&0x03)
	header[4] = byte((totalLen >> 3) & 0xFF)
	header[5] = byte((totalLen<<5)&0xE0) | 0x1F
	header[6] = 0xFC
	return header
}
