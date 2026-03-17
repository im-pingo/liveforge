package opus

import (
	"encoding/binary"
	"testing"
)

func TestParseOpusHead(t *testing.T) {
	head := make([]byte, 19)
	copy(head[0:8], "OpusHead")
	head[8] = 1 // version
	head[9] = 2 // channels
	binary.LittleEndian.PutUint16(head[10:12], 312)
	binary.LittleEndian.PutUint32(head[12:16], 48000)
	binary.LittleEndian.PutUint16(head[16:18], 0)
	head[18] = 0

	info, err := ParseOpusHead(head)
	if err != nil {
		t.Fatal(err)
	}
	if info.Channels != 2 {
		t.Errorf("expected 2 channels, got %d", info.Channels)
	}
	if info.SampleRate != 48000 {
		t.Errorf("expected 48000, got %d", info.SampleRate)
	}
	if info.PreSkip != 312 {
		t.Errorf("expected pre-skip 312, got %d", info.PreSkip)
	}
}

func TestParseOpusHeadInvalid(t *testing.T) {
	_, err := ParseOpusHead([]byte{1, 2, 3})
	if err == nil {
		t.Error("expected error for short data")
	}

	bad := make([]byte, 19)
	copy(bad[0:8], "NotOpus!")
	_, err = ParseOpusHead(bad)
	if err == nil {
		t.Error("expected error for bad magic")
	}
}

func TestBuildDOpsBox(t *testing.T) {
	info := &OpusInfo{Channels: 2, PreSkip: 312, SampleRate: 48000}
	dops := BuildDOpsBox(info)
	if len(dops) != 11 {
		t.Fatalf("expected 11 bytes, got %d", len(dops))
	}
	if dops[1] != 2 {
		t.Error("channels mismatch")
	}
}
