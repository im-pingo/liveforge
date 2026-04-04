package gb28181

import "time"

// DeviceStatus represents the status of a GB28181 device.
type DeviceStatus uint8

const (
	DeviceStatusOnline  DeviceStatus = iota + 1
	DeviceStatusOffline
)

func (s DeviceStatus) String() string {
	switch s {
	case DeviceStatusOnline:
		return "online"
	case DeviceStatusOffline:
		return "offline"
	default:
		return "unknown"
	}
}

// Device represents a registered GB28181 device.
type Device struct {
	DeviceID      string
	RemoteAddr    string
	Transport     string
	RegisteredAt  time.Time
	LastKeepalive time.Time
	Status        DeviceStatus
	Channels      map[string]*Channel
}

// Channel represents a media channel on a GB28181 device.
type Channel struct {
	ChannelID    string
	Name         string
	Manufacturer string
	Status       string
	PTZType      int
	Latitude     float64
	Longitude    float64
}

// SessionDirection indicates the direction of a media session.
type SessionDirection uint8

const (
	SessionDirectionInbound  SessionDirection = iota + 1 // device pushes to us
	SessionDirectionOutbound                             // we invite device
)

// SessionState represents the state of a media session.
type SessionState uint8

const (
	SessionStateIdle      SessionState = iota
	SessionStateInviting
	SessionStateStreaming
	SessionStateClosed
)

func (s SessionState) String() string {
	switch s {
	case SessionStateIdle:
		return "idle"
	case SessionStateInviting:
		return "inviting"
	case SessionStateStreaming:
		return "streaming"
	case SessionStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}
