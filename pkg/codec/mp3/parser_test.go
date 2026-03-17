package mp3

import "testing"

func TestParseFrameHeader(t *testing.T) {
	// 128kbps, 44100Hz, stereo: 0xFF 0xFB 0x90 0x00
	header := []byte{0xFF, 0xFB, 0x90, 0x00}
	info, err := ParseFrameHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if info.SampleRate != 44100 {
		t.Errorf("expected 44100, got %d", info.SampleRate)
	}
	if info.Channels != 2 {
		t.Errorf("expected 2 channels, got %d", info.Channels)
	}
	if info.Bitrate != 128000 {
		t.Errorf("expected 128000, got %d", info.Bitrate)
	}
	if info.Version != 1 {
		t.Errorf("expected MPEG1, got %d", info.Version)
	}
	if info.Layer != 3 {
		t.Errorf("expected layer 3, got %d", info.Layer)
	}
}

func TestParseFrameHeaderMono(t *testing.T) {
	// mono mode: channel mode bits = 11 (0xC0 in byte 3)
	header := []byte{0xFF, 0xFB, 0x90, 0xC0}
	info, err := ParseFrameHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if info.Channels != 1 {
		t.Errorf("expected 1 channel, got %d", info.Channels)
	}
}

func TestParseFrameHeaderInvalid(t *testing.T) {
	_, err := ParseFrameHeader([]byte{0x00, 0x00, 0x00, 0x00})
	if err == nil {
		t.Error("expected error for invalid sync")
	}

	_, err = ParseFrameHeader([]byte{0xFF})
	if err == nil {
		t.Error("expected error for short data")
	}
}
