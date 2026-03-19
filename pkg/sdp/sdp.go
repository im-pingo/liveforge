package sdp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SessionDescription represents a parsed SDP session.
type SessionDescription struct {
	Version    int
	Origin     Origin
	Name       string
	Info       string
	Connection *Connection
	Timing     Timing
	Attributes []Attribute
	Media      []*MediaDescription
}

// Origin contains the o= line fields.
type Origin struct {
	Username       string
	SessionID      string
	SessionVersion string
	NetType        string
	AddrType       string
	Address        string
}

// Connection contains the c= line fields.
type Connection struct {
	NetType  string
	AddrType string
	Address  string
}

// Timing contains the t= line fields.
type Timing struct {
	Start uint64
	Stop  uint64
}

// Attribute represents a session-level or media-level attribute (a= line).
type Attribute struct {
	Key   string
	Value string
}

// MediaDescription represents a single m= section and its attributes.
type MediaDescription struct {
	Type       string
	Port       int
	Proto      string
	Formats    []int
	Connection *Connection
	Bandwidth  string
	Attributes []Attribute
}

// RTPMapInfo holds parsed rtpmap attribute data.
type RTPMapInfo struct {
	PayloadType  int
	EncodingName string
	ClockRate    int
	Channels     int
}

// RTPMap returns the parsed rtpmap for the given payload type, or nil if not found.
func (md *MediaDescription) RTPMap(pt int) *RTPMapInfo {
	prefix := fmt.Sprintf("%d ", pt)
	for _, a := range md.Attributes {
		if a.Key != "rtpmap" {
			continue
		}
		if !strings.HasPrefix(a.Value, prefix) {
			continue
		}
		// Format: "<pt> <encoding>/<clock>[/<channels>]"
		rest := a.Value[len(prefix):]
		parts := strings.Split(rest, "/")
		if len(parts) < 2 {
			continue
		}
		clock, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		info := &RTPMapInfo{
			PayloadType:  pt,
			EncodingName: parts[0],
			ClockRate:    clock,
		}
		if len(parts) >= 3 {
			ch, err := strconv.Atoi(parts[2])
			if err == nil {
				info.Channels = ch
			}
		}
		return info
	}
	return nil
}

// FMTP returns the fmtp parameter string for the given payload type, or empty if not found.
func (md *MediaDescription) FMTP(pt int) string {
	prefix := fmt.Sprintf("%d ", pt)
	for _, a := range md.Attributes {
		if a.Key == "fmtp" && strings.HasPrefix(a.Value, prefix) {
			return a.Value[len(prefix):]
		}
	}
	return ""
}

// Control returns the value of the a=control attribute, or empty if not present.
func (md *MediaDescription) Control() string {
	for _, a := range md.Attributes {
		if a.Key == "control" {
			return a.Value
		}
	}
	return ""
}

// Direction returns the media direction (sendrecv, sendonly, recvonly, inactive).
// Defaults to "sendrecv" if no direction attribute is present.
func (md *MediaDescription) Direction() string {
	for _, a := range md.Attributes {
		switch a.Key {
		case "sendrecv", "sendonly", "recvonly", "inactive":
			return a.Key
		}
	}
	return "sendrecv"
}

// Parse parses raw SDP text into a SessionDescription.
func Parse(data []byte) (*SessionDescription, error) {
	if len(data) == 0 {
		return nil, errors.New("sdp: empty input")
	}

	text := string(data)
	// Normalize line endings: \r\n -> \n, then split.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")

	sd := &SessionDescription{Version: -1}
	var currentMedia *MediaDescription

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) < 2 || line[1] != '=' {
			continue
		}
		key := line[0]
		value := line[2:]

		switch key {
		case 'v':
			v, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("sdp: invalid version %q: %w", value, err)
			}
			sd.Version = v

		case 'o':
			o, err := parseOrigin(value)
			if err != nil {
				return nil, err
			}
			sd.Origin = o

		case 's':
			sd.Name = value

		case 'i':
			if currentMedia == nil {
				sd.Info = value
			}

		case 'c':
			c, err := parseConnection(value)
			if err != nil {
				return nil, err
			}
			if currentMedia != nil {
				currentMedia.Connection = &c
			} else {
				sd.Connection = &c
			}

		case 't':
			t, err := parseTiming(value)
			if err != nil {
				return nil, err
			}
			sd.Timing = t

		case 'b':
			if currentMedia != nil {
				currentMedia.Bandwidth = value
			}

		case 'm':
			m, err := parseMedia(value)
			if err != nil {
				return nil, err
			}
			sd.Media = append(sd.Media, &m)
			currentMedia = &m
			// Update the slice element (since append may copy).
			currentMedia = sd.Media[len(sd.Media)-1]

		case 'a':
			attr := parseAttribute(value)
			if currentMedia != nil {
				currentMedia.Attributes = append(currentMedia.Attributes, attr)
			} else {
				sd.Attributes = append(sd.Attributes, attr)
			}
		}
	}

	if sd.Version < 0 {
		return nil, errors.New("sdp: missing v= line")
	}

	return sd, nil
}

// Marshal serializes a SessionDescription back to SDP text.
func (sd *SessionDescription) Marshal() []byte {
	var b strings.Builder

	fmt.Fprintf(&b, "v=%d\r\n", sd.Version)
	fmt.Fprintf(&b, "o=%s %s %s %s %s %s\r\n",
		sd.Origin.Username, sd.Origin.SessionID, sd.Origin.SessionVersion,
		sd.Origin.NetType, sd.Origin.AddrType, sd.Origin.Address)
	fmt.Fprintf(&b, "s=%s\r\n", sd.Name)

	if sd.Info != "" {
		fmt.Fprintf(&b, "i=%s\r\n", sd.Info)
	}
	if sd.Connection != nil {
		fmt.Fprintf(&b, "c=%s %s %s\r\n",
			sd.Connection.NetType, sd.Connection.AddrType, sd.Connection.Address)
	}

	fmt.Fprintf(&b, "t=%d %d\r\n", sd.Timing.Start, sd.Timing.Stop)

	for _, a := range sd.Attributes {
		marshalAttribute(&b, a)
	}

	for _, m := range sd.Media {
		fmts := make([]string, len(m.Formats))
		for i, f := range m.Formats {
			fmts[i] = strconv.Itoa(f)
		}
		fmt.Fprintf(&b, "m=%s %d %s %s\r\n", m.Type, m.Port, m.Proto, strings.Join(fmts, " "))

		if m.Connection != nil {
			fmt.Fprintf(&b, "c=%s %s %s\r\n",
				m.Connection.NetType, m.Connection.AddrType, m.Connection.Address)
		}
		if m.Bandwidth != "" {
			fmt.Fprintf(&b, "b=%s\r\n", m.Bandwidth)
		}
		for _, a := range m.Attributes {
			marshalAttribute(&b, a)
		}
	}

	return []byte(b.String())
}

func marshalAttribute(b *strings.Builder, a Attribute) {
	if a.Value == "" {
		fmt.Fprintf(b, "a=%s\r\n", a.Key)
	} else {
		fmt.Fprintf(b, "a=%s:%s\r\n", a.Key, a.Value)
	}
}

func parseOrigin(value string) (Origin, error) {
	// "<username> <session-id> <version> <nettype> <addrtype> <address>"
	parts := strings.Fields(value)
	if len(parts) < 6 {
		return Origin{}, fmt.Errorf("sdp: invalid origin %q", value)
	}
	return Origin{
		Username:       parts[0],
		SessionID:      parts[1],
		SessionVersion: parts[2],
		NetType:        parts[3],
		AddrType:       parts[4],
		Address:        parts[5],
	}, nil
}

func parseConnection(value string) (Connection, error) {
	// "<nettype> <addrtype> <address>"
	parts := strings.Fields(value)
	if len(parts) < 3 {
		return Connection{}, fmt.Errorf("sdp: invalid connection %q", value)
	}
	return Connection{
		NetType:  parts[0],
		AddrType: parts[1],
		Address:  parts[2],
	}, nil
}

func parseTiming(value string) (Timing, error) {
	parts := strings.Fields(value)
	if len(parts) < 2 {
		return Timing{}, fmt.Errorf("sdp: invalid timing %q", value)
	}
	start, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return Timing{}, fmt.Errorf("sdp: invalid timing start %q: %w", parts[0], err)
	}
	stop, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return Timing{}, fmt.Errorf("sdp: invalid timing stop %q: %w", parts[1], err)
	}
	return Timing{Start: start, Stop: stop}, nil
}

func parseMedia(value string) (MediaDescription, error) {
	// "<type> <port> <proto> <fmt1> [fmt2 ...]"
	parts := strings.Fields(value)
	if len(parts) < 4 {
		return MediaDescription{}, fmt.Errorf("sdp: invalid media %q", value)
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		return MediaDescription{}, fmt.Errorf("sdp: invalid media port %q: %w", parts[1], err)
	}
	formats := make([]int, 0, len(parts)-3)
	for _, f := range parts[3:] {
		pt, err := strconv.Atoi(f)
		if err != nil {
			return MediaDescription{}, fmt.Errorf("sdp: invalid format %q: %w", f, err)
		}
		formats = append(formats, pt)
	}
	return MediaDescription{
		Type:    parts[0],
		Port:    port,
		Proto:   parts[2],
		Formats: formats,
	}, nil
}

func parseAttribute(value string) Attribute {
	// "key:value" or just "key" (flag attribute)
	idx := strings.IndexByte(value, ':')
	if idx < 0 {
		return Attribute{Key: value}
	}
	return Attribute{Key: value[:idx], Value: value[idx+1:]}
}
