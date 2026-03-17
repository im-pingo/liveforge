package mp3

import "fmt"

// MP3Info holds parsed MP3 frame header information.
type MP3Info struct {
	SampleRate int
	Channels   int
	Bitrate    int
	Version    int // 1=MPEG1, 2=MPEG2, 3=MPEG2.5
	Layer      int // 1, 2, or 3
}

var mp3SampleRates = [3][3]int{
	{44100, 48000, 32000},
	{22050, 24000, 16000},
	{11025, 12000, 8000},
}

var mp3Bitrates = [2][3][15]int{
	{{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}},
	{{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}},
}

// ParseFrameHeader parses a 4-byte MP3 frame header.
func ParseFrameHeader(data []byte) (*MP3Info, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("MP3 header too short")
	}
	if data[0] != 0xFF || (data[1]&0xE0) != 0xE0 {
		return nil, fmt.Errorf("invalid MP3 sync word")
	}

	versionBits := (data[1] >> 3) & 0x03
	layerBits := (data[1] >> 1) & 0x03
	bitrateIdx := (data[2] >> 4) & 0x0F
	srIdx := (data[2] >> 2) & 0x03
	channelMode := (data[3] >> 6) & 0x03

	var versionIdx, version int
	switch versionBits {
	case 3:
		versionIdx = 0
		version = 1
	case 2:
		versionIdx = 1
		version = 2
	case 0:
		versionIdx = 2
		version = 3
	default:
		return nil, fmt.Errorf("reserved MPEG version")
	}

	var layer int
	switch layerBits {
	case 3:
		layer = 1
	case 2:
		layer = 2
	case 1:
		layer = 3
	default:
		return nil, fmt.Errorf("reserved layer")
	}

	if srIdx > 2 {
		return nil, fmt.Errorf("reserved sample rate index")
	}
	sampleRate := mp3SampleRates[versionIdx][srIdx]

	channels := 2
	if channelMode == 3 {
		channels = 1
	}

	bitrateRow := 0
	if versionIdx > 0 {
		bitrateRow = 1
	}
	bitrate := 0
	if bitrateIdx > 0 && bitrateIdx < 15 {
		bitrate = mp3Bitrates[bitrateRow][layer-1][bitrateIdx] * 1000
	}

	return &MP3Info{
		SampleRate: sampleRate,
		Channels:   channels,
		Bitrate:    bitrate,
		Version:    version,
		Layer:      layer,
	}, nil
}
