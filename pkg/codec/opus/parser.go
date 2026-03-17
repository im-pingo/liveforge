package opus

import (
	"encoding/binary"
	"fmt"
)

// OpusInfo holds parsed OpusHead information.
type OpusInfo struct {
	Version    int
	Channels   int
	PreSkip    int
	SampleRate int
}

// ParseOpusHead parses an OpusHead identification header.
func ParseOpusHead(data []byte) (*OpusInfo, error) {
	if len(data) < 19 {
		return nil, fmt.Errorf("OpusHead too short: %d bytes", len(data))
	}
	if string(data[0:8]) != "OpusHead" {
		return nil, fmt.Errorf("invalid OpusHead magic: %q", data[0:8])
	}
	return &OpusInfo{
		Version:    int(data[8]),
		Channels:   int(data[9]),
		PreSkip:    int(binary.LittleEndian.Uint16(data[10:12])),
		SampleRate: int(binary.LittleEndian.Uint32(data[12:16])),
	}, nil
}

// BuildDOpsBox builds the dOps box content for FMP4 from OpusInfo.
func BuildDOpsBox(info *OpusInfo) []byte {
	buf := make([]byte, 11)
	buf[0] = 0 // version
	buf[1] = byte(info.Channels)
	binary.BigEndian.PutUint16(buf[2:4], uint16(info.PreSkip))
	binary.BigEndian.PutUint32(buf[4:8], uint32(info.SampleRate))
	binary.BigEndian.PutUint16(buf[8:10], 0) // output gain
	buf[10] = 0                               // channel mapping family
	return buf
}
