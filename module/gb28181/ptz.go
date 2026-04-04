package gb28181

import "fmt"

// PTZ command byte positions per GB28181 Annex A.
// Format: A5 0F 01 [cmd1] [cmd2] [speed1] [speed2] [checksum]
// - Byte 0: 0xA5 (fixed)
// - Byte 1: 0x0F (fixed, version + unused)
// - Byte 2: 0x01 (address, lower 4 bits)
// - Byte 3: cmd1 (direction control)
// - Byte 4: cmd2 (zoom control)
// - Byte 5: horizontal speed (0x00-0xFF)
// - Byte 6: vertical speed (0x00-0xFF)
// - Byte 7: checksum (sum of bytes 0-6, lower 8 bits)

// PTZDirection represents a PTZ movement direction.
type PTZDirection uint8

const (
	PTZUp        PTZDirection = 0x08
	PTZDown      PTZDirection = 0x04
	PTZLeft      PTZDirection = 0x02
	PTZRight     PTZDirection = 0x01
	PTZUpLeft    PTZDirection = 0x0A
	PTZUpRight   PTZDirection = 0x09
	PTZDownLeft  PTZDirection = 0x06
	PTZDownRight PTZDirection = 0x05
	PTZStop      PTZDirection = 0x00
)

// PTZZoom represents a zoom operation.
type PTZZoom uint8

const (
	PTZZoomIn  PTZZoom = 0x10
	PTZZoomOut PTZZoom = 0x20
	PTZZoomStop PTZZoom = 0x00
)

// BuildPTZCmd generates the 8-byte hex-encoded PTZ command string.
func BuildPTZCmd(direction PTZDirection, zoom PTZZoom, hSpeed, vSpeed uint8) string {
	cmd := [8]byte{
		0xA5,
		0x0F,
		0x01,
		byte(direction),
		byte(zoom),
		hSpeed,
		vSpeed,
		0, // checksum
	}

	// Checksum = sum of bytes 0-6, lower 8 bits
	var sum byte
	for i := 0; i < 7; i++ {
		sum += cmd[i]
	}
	cmd[7] = sum

	return fmt.Sprintf("%02X%02X%02X%02X%02X%02X%02X%02X",
		cmd[0], cmd[1], cmd[2], cmd[3], cmd[4], cmd[5], cmd[6], cmd[7])
}

// BuildPTZStopCmd generates a stop command.
func BuildPTZStopCmd() string {
	return BuildPTZCmd(PTZStop, PTZZoomStop, 0, 0)
}
