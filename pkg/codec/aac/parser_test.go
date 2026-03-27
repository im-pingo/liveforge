package aac

import "testing"

func TestBuildADTSHeader(t *testing.T) {
	info := &AACInfo{ObjectType: 2, SampleRate: 44100, Channels: 2}
	header := BuildADTSHeader(info, 100)
	if len(header) != 7 {
		t.Fatalf("expected 7 bytes, got %d", len(header))
	}
	if header[0] != 0xFF || (header[1]&0xF0) != 0xF0 {
		t.Error("invalid ADTS sync word")
	}
	// Verify total length is encoded correctly
	totalLen := 7 + 100
	encodedLen := (int(header[3]&0x03) << 11) | (int(header[4]) << 3) | (int(header[5]&0xE0) >> 5)
	if encodedLen != totalLen {
		t.Errorf("expected total length %d, got %d", totalLen, encodedLen)
	}
}

func TestSampleRateIndex(t *testing.T) {
	if SampleRateIndex(44100) != 4 {
		t.Error("44100 should be index 4")
	}
	if SampleRateIndex(48000) != 3 {
		t.Error("48000 should be index 3")
	}
	if SampleRateIndex(99999) != 0x0F {
		t.Error("unknown rate should be 0x0F")
	}
}

func TestParseADTSHeader(t *testing.T) {
	// Build an ADTS header for AAC-LC, 44100Hz, stereo, 100-byte frame
	info := &AACInfo{ObjectType: 2, SampleRate: 44100, Channels: 2}
	header := BuildADTSHeader(info, 100)

	parsed, headerLen, err := ParseADTSHeader(header)
	if err != nil {
		t.Fatalf("ParseADTSHeader error: %v", err)
	}
	if headerLen != 7 {
		t.Errorf("expected header length 7, got %d", headerLen)
	}
	if parsed.ObjectType != 2 {
		t.Errorf("expected object type 2, got %d", parsed.ObjectType)
	}
	if parsed.SampleRate != 44100 {
		t.Errorf("expected sample rate 44100, got %d", parsed.SampleRate)
	}
	if parsed.Channels != 2 {
		t.Errorf("expected channels 2, got %d", parsed.Channels)
	}
}

func TestParseADTSHeaderTooShort(t *testing.T) {
	_, _, err := ParseADTSHeader([]byte{0xFF, 0xF1})
	if err == nil {
		t.Error("expected error for short data")
	}
}

func TestParseADTSHeaderBadSync(t *testing.T) {
	_, _, err := ParseADTSHeader([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	if err == nil {
		t.Error("expected error for bad sync word")
	}
}

func TestBuildAudioSpecificConfig(t *testing.T) {
	info := &AACInfo{ObjectType: 2, SampleRate: 44100, Channels: 2}
	asc := BuildAudioSpecificConfig(info)
	if len(asc) != 2 {
		t.Fatalf("expected 2 bytes, got %d", len(asc))
	}
	// Round-trip: parse the ASC we just built
	parsed, err := ParseAudioSpecificConfig(asc)
	if err != nil {
		t.Fatalf("ParseAudioSpecificConfig error: %v", err)
	}
	if parsed.ObjectType != 2 {
		t.Errorf("expected object type 2, got %d", parsed.ObjectType)
	}
	if parsed.SampleRate != 44100 {
		t.Errorf("expected sample rate 44100, got %d", parsed.SampleRate)
	}
	if parsed.Channels != 2 {
		t.Errorf("expected channels 2, got %d", parsed.Channels)
	}
}

func TestBuildAudioSpecificConfig48kHz(t *testing.T) {
	info := &AACInfo{ObjectType: 2, SampleRate: 48000, Channels: 6}
	asc := BuildAudioSpecificConfig(info)
	parsed, err := ParseAudioSpecificConfig(asc)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if parsed.SampleRate != 48000 || parsed.Channels != 6 {
		t.Errorf("round-trip failed: got rate=%d channels=%d", parsed.SampleRate, parsed.Channels)
	}
}

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
