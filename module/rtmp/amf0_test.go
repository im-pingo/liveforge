package rtmp

import (
	"bytes"
	"testing"
)

func TestAMF0RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		vals []any
	}{
		{"string", []any{"hello"}},
		{"number", []any{float64(42)}},
		{"boolean", []any{true}},
		{"null", []any{nil}},
		{"mixed", []any{"connect", float64(1.0), nil}},
		{"object", []any{map[string]any{"app": "live", "type": "nonprivate"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := AMF0Encode(tt.vals...)
			if err != nil {
				t.Fatalf("encode error: %v", err)
			}

			decoded, err := AMF0Decode(encoded)
			if err != nil {
				t.Fatalf("decode error: %v", err)
			}

			if len(decoded) != len(tt.vals) {
				t.Fatalf("expected %d values, got %d", len(tt.vals), len(decoded))
			}

			for i, want := range tt.vals {
				got := decoded[i]
				switch w := want.(type) {
				case string:
					if g, ok := got.(string); !ok || g != w {
						t.Errorf("[%d] expected %q, got %v", i, w, got)
					}
				case float64:
					if g, ok := got.(float64); !ok || g != w {
						t.Errorf("[%d] expected %f, got %v", i, w, got)
					}
				case bool:
					if g, ok := got.(bool); !ok || g != w {
						t.Errorf("[%d] expected %v, got %v", i, w, got)
					}
				case nil:
					if got != nil {
						t.Errorf("[%d] expected nil, got %v", i, got)
					}
				case map[string]any:
					g, ok := got.(map[string]any)
					if !ok {
						t.Errorf("[%d] expected map, got %T", i, got)
						continue
					}
					for k, v := range w {
						if g[k] != v {
							t.Errorf("[%d] key %s: expected %v, got %v", i, k, v, g[k])
						}
					}
				}
			}
		})
	}
}

func TestAMF0EncodeEmpty(t *testing.T) {
	encoded, err := AMF0Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != 0 {
		t.Errorf("expected empty, got %d bytes", len(encoded))
	}
}

func TestAMF0DecodeConnectCommand(t *testing.T) {
	// Simulate a typical RTMP connect command
	encoded, err := AMF0Encode(
		"connect",
		float64(1),
		map[string]any{
			"app":      "live",
			"flashVer": "FMLE/3.0",
			"tcUrl":    "rtmp://localhost/live",
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	vals, err := AMF0Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}

	if len(vals) != 3 {
		t.Fatalf("expected 3 values, got %d", len(vals))
	}
	if vals[0] != "connect" {
		t.Errorf("expected 'connect', got %v", vals[0])
	}

	obj, ok := vals[2].(map[string]any)
	if !ok {
		t.Fatalf("expected map, got %T", vals[2])
	}
	if obj["app"] != "live" {
		t.Errorf("expected app=live, got %v", obj["app"])
	}
}

func TestAMF0DecodeEmptyInput(t *testing.T) {
	vals, err := AMF0Decode([]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 0 {
		t.Errorf("expected 0 values, got %d", len(vals))
	}
}

// Test that encoded bytes are not accidentally modified
func TestAMF0EncodeDeterministic(t *testing.T) {
	encoded1, _ := AMF0Encode("test", float64(42))
	encoded2, _ := AMF0Encode("test", float64(42))
	if !bytes.Equal(encoded1, encoded2) {
		t.Error("encoding should be deterministic")
	}
}

func TestAMF0StrictArrayRoundTrip(t *testing.T) {
	want := []any{"vp08", "vp09", "av01", "hvc1", "Opus"}
	encoded, err := AMF0Encode(map[string]any{"fourCcList": want})
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}
	decoded, err := AMF0Decode(encoded)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	obj, ok := decoded[0].(map[string]any)
	if !ok {
		t.Fatalf("decoded object type = %T", decoded[0])
	}
	got, ok := obj["fourCcList"].([]any)
	if !ok {
		t.Fatalf("decoded array type = %T", obj["fourCcList"])
	}
	if len(got) != len(want) {
		t.Fatalf("array length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("array[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestAMF0ECMAArrayDecode(t *testing.T) {
	data := []byte{
		0x08,                   // ECMA array
		0x00, 0x00, 0x00, 0x01, // one property
		0x00, 0x12, // videoFourCcInfoMap
		'v', 'i', 'd', 'e', 'o', 'F', 'o', 'u', 'r', 'C', 'c', 'I', 'n', 'f', 'o', 'M', 'a', 'p',
		0x03, // object value
		0x00, 0x04, 'v', 'p', '0', '9',
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // number 0
		0x00, 0x00, 0x09, // nested object end
		0x00, 0x00, 0x09, // ECMA array end
	}
	decoded, err := AMF0Decode(data)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("decoded values = %d, want 1", len(decoded))
	}
	obj, ok := decoded[0].(map[string]any)
	if !ok {
		t.Fatalf("decoded type = %T", decoded[0])
	}
	if _, ok := obj["videoFourCcInfoMap"].(map[string]any); !ok {
		t.Fatalf("nested ECMA/object type = %T", obj["videoFourCcInfoMap"])
	}
}
