package aac

import "testing"

func TestParseAudioSpecificConfig(t *testing.T) {
	// AAC-LC, 44100Hz, stereo: object_type=2, freq_index=4, channel=2
	// Binary: 00010 0100 0010 000 = 0x12 0x10
	config := []byte{0x12, 0x10}
	info, err := ParseAudioSpecificConfig(config)
	if err != nil {
		t.Fatalf("ParseAudioSpecificConfig error: %v", err)
	}
	if info.ObjectType != 2 {
		t.Errorf("expected object type 2 (AAC-LC), got %d", info.ObjectType)
	}
	if info.SampleRate != 44100 {
		t.Errorf("expected sample rate 44100, got %d", info.SampleRate)
	}
	if info.Channels != 2 {
		t.Errorf("expected channels 2, got %d", info.Channels)
	}
}
