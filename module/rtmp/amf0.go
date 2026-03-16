package rtmp

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// AMF0 type markers.
const (
	amf0Number    byte = 0x00
	amf0Boolean   byte = 0x01
	amf0String    byte = 0x02
	amf0Object    byte = 0x03
	amf0Null      byte = 0x05
	amf0ObjectEnd byte = 0x09
)

// AMF0Encode encodes Go values to AMF0 bytes.
func AMF0Encode(vals ...any) ([]byte, error) {
	var buf []byte
	for _, v := range vals {
		encoded, err := amf0EncodeValue(v)
		if err != nil {
			return nil, err
		}
		buf = append(buf, encoded...)
	}
	return buf, nil
}

// AMF0Decode decodes AMF0 bytes to Go values.
func AMF0Decode(data []byte) ([]any, error) {
	var vals []any
	offset := 0
	for offset < len(data) {
		val, n, err := amf0DecodeValue(data[offset:])
		if err != nil {
			return nil, err
		}
		vals = append(vals, val)
		offset += n
	}
	return vals, nil
}

func amf0EncodeValue(v any) ([]byte, error) {
	switch val := v.(type) {
	case string:
		return amf0EncodeString(val), nil
	case float64:
		return amf0EncodeNumber(val), nil
	case bool:
		return amf0EncodeBool(val), nil
	case nil:
		return []byte{amf0Null}, nil
	case map[string]any:
		return amf0EncodeObject(val), nil
	default:
		return nil, fmt.Errorf("unsupported AMF0 type: %T", v)
	}
}

func amf0EncodeString(s string) []byte {
	buf := make([]byte, 1+2+len(s))
	buf[0] = amf0String
	binary.BigEndian.PutUint16(buf[1:3], uint16(len(s)))
	copy(buf[3:], s)
	return buf
}

func amf0EncodeNumber(n float64) []byte {
	buf := make([]byte, 9)
	buf[0] = amf0Number
	binary.BigEndian.PutUint64(buf[1:], math.Float64bits(n))
	return buf
}

func amf0EncodeBool(b bool) []byte {
	buf := []byte{amf0Boolean, 0}
	if b {
		buf[1] = 1
	}
	return buf
}

func amf0EncodeObject(obj map[string]any) []byte {
	var buf []byte
	buf = append(buf, amf0Object)

	// Sort keys for deterministic output
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		// Property name (without type marker)
		buf = append(buf, byte(len(k)>>8), byte(len(k)))
		buf = append(buf, k...)
		// Property value
		encoded, _ := amf0EncodeValue(obj[k])
		buf = append(buf, encoded...)
	}

	// Object end marker: 0x00 0x00 0x09
	buf = append(buf, 0x00, 0x00, amf0ObjectEnd)
	return buf
}

func amf0DecodeValue(data []byte) (any, int, error) {
	if len(data) < 1 {
		return nil, 0, fmt.Errorf("AMF0: empty data")
	}

	switch data[0] {
	case amf0Number:
		if len(data) < 9 {
			return nil, 0, fmt.Errorf("AMF0 number: need 9 bytes, got %d", len(data))
		}
		bits := binary.BigEndian.Uint64(data[1:9])
		return math.Float64frombits(bits), 9, nil

	case amf0Boolean:
		if len(data) < 2 {
			return nil, 0, fmt.Errorf("AMF0 boolean: need 2 bytes")
		}
		return data[1] != 0, 2, nil

	case amf0String:
		if len(data) < 3 {
			return nil, 0, fmt.Errorf("AMF0 string: need at least 3 bytes")
		}
		slen := int(binary.BigEndian.Uint16(data[1:3]))
		if len(data) < 3+slen {
			return nil, 0, fmt.Errorf("AMF0 string: need %d bytes, got %d", 3+slen, len(data))
		}
		return string(data[3 : 3+slen]), 3 + slen, nil

	case amf0Null:
		return nil, 1, nil

	case amf0Object:
		return amf0DecodeObject(data[1:])

	default:
		return nil, 0, fmt.Errorf("AMF0: unsupported type marker 0x%02x", data[0])
	}
}

func amf0DecodeObject(data []byte) (map[string]any, int, error) {
	obj := make(map[string]any)
	offset := 0

	for {
		if offset+3 > len(data) {
			return nil, 0, fmt.Errorf("AMF0 object: unexpected end")
		}

		// Check for object end marker
		if data[offset] == 0x00 && data[offset+1] == 0x00 && data[offset+2] == amf0ObjectEnd {
			return obj, 1 + offset + 3, nil // +1 for the initial amf0Object marker
		}

		// Property name
		nameLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		offset += 2
		if offset+nameLen > len(data) {
			return nil, 0, fmt.Errorf("AMF0 object: property name overflow")
		}
		name := string(data[offset : offset+nameLen])
		offset += nameLen

		// Property value
		val, n, err := amf0DecodeValue(data[offset:])
		if err != nil {
			return nil, 0, fmt.Errorf("AMF0 object property %q: %w", name, err)
		}
		obj[name] = val
		offset += n
	}
}
