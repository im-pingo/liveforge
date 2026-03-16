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
