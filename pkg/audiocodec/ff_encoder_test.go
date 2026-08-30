//go:build audiocodec

package audiocodec

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

var _ DrainingEncoder = (*FFmpegEncoder)(nil)
var _ AttributedDrainingEncoder = (*FFmpegEncoder)(nil)

func TestFFmpegEncoderPCMU(t *testing.T) {
	enc := NewFFmpegEncoder("pcm_mulaw", 8000, 1)
	defer enc.Close()
	if enc.SampleRate() != 8000 {
		t.Fatalf("expected 8000, got %d", enc.SampleRate())
	}
	if enc.Channels() != 1 {
		t.Fatalf("expected 1, got %d", enc.Channels())
	}
	pcm := &PCMFrame{Samples: make([]int16, 160), SampleRate: 8000, Channels: 1}
	payload, err := enc.Encode(pcm)
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}
	if len(payload) != 160 {
		t.Fatalf("expected 160 bytes, got %d", len(payload))
	}
}

func TestFFmpegEncoderDecodeRoundTrip(t *testing.T) {
	enc := NewFFmpegEncoder("pcm_mulaw", 8000, 1)
	defer enc.Close()
	dec := NewFFmpegDecoder("pcm_mulaw")
	defer dec.Close()
	original := &PCMFrame{Samples: make([]int16, 160), SampleRate: 8000, Channels: 1}
	for i := range original.Samples {
		original.Samples[i] = int16(i * 100)
	}
	encoded, err := enc.Encode(original)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := dec.Decode(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Samples) != 160 {
		t.Fatalf("expected 160 samples, got %d", len(decoded.Samples))
	}
	for i := 0; i < 10; i++ {
		diff := int(original.Samples[i]) - int(decoded.Samples[i])
		if diff < -200 || diff > 200 {
			t.Errorf("sample %d: original=%d decoded=%d diff=%d",
				i, original.Samples[i], decoded.Samples[i], diff)
		}
	}
}

func TestFFmpegEncoderDrainReturnsDelayedAACPacketsExactlyOnce(t *testing.T) {
	enc := NewFFmpegEncoder("aac", 48000, 2)
	defer enc.Close()

	frameSize := enc.FrameSize()
	if frameSize <= 0 {
		t.Fatalf("AAC encoder frame size = %d, want fixed frame size", frameSize)
	}
	pcm := &PCMFrame{
		Samples:    make([]int16, frameSize*enc.Channels()),
		SampleRate: enc.SampleRate(),
		Channels:   enc.Channels(),
	}
	for i := range pcm.Samples {
		pcm.Samples[i] = int16((i%257 - 128) * 128)
	}

	beforeDrain, err := enc.Encode(pcm)
	if err != nil {
		t.Fatalf("encode AAC frame: %v", err)
	}
	if len(beforeDrain) == 0 {
		t.Fatal("encode returned no primed AAC packet before terminal drain")
	}

	drainer, ok := any(enc).(DrainingEncoder)
	if !ok {
		t.Fatal("FFmpeg encoder does not expose terminal drain")
	}
	delayed, err := drainer.Drain()
	if err != nil {
		t.Fatalf("drain AAC encoder: %v", err)
	}
	if len(delayed) == 0 {
		t.Fatal("drain returned no delayed AAC packets")
	}
	seen := map[string]int{string(beforeDrain): -1}
	for i, packet := range delayed {
		if len(packet) == 0 {
			t.Fatalf("drained AAC packet %d is empty", i)
		}
		if previous, duplicate := seen[string(packet)]; duplicate {
			t.Fatalf("drained AAC packet %d duplicates packet %d; a missing packet could be masked by repeated output", i, previous)
		}
		seen[string(packet)] = i
	}

	again, err := drainer.Drain()
	if err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second drain returned %d duplicate AAC packets, want 0", len(again))
	}

	enc.Close()
	for i, packet := range delayed {
		if len(packet) == 0 {
			t.Fatalf("drained AAC packet %d was not retained in Go memory after close", i)
		}
	}
}

func TestFFmpegEncoderDrainRetriesTerminalSendAfterEAGAIN(t *testing.T) {
	enc := &FFmpegEncoder{codecName: "scripted-aac"}
	sendCalls := 0
	sendEOF := func() error {
		sendCalls++
		if sendCalls == 1 {
			return errFFmpegDrainAgain
		}
		return nil
	}

	type receiveStep struct {
		packet []byte
		err    error
	}
	steps := []receiveStep{
		{packet: []byte{0x10}},
		{packet: []byte{0x20}},
		{err: errFFmpegDrainAgain},
		{packet: []byte{0x30}},
		{err: io.EOF},
	}
	receiveCalls := 0
	receive := func() ([]byte, error) {
		if receiveCalls >= len(steps) {
			t.Fatal("drain received beyond scripted EOF")
		}
		step := steps[receiveCalls]
		receiveCalls++
		return append([]byte(nil), step.packet...), step.err
	}

	packets, err := enc.drainWith(sendEOF, receive)
	if err != nil {
		t.Fatalf("drain after send-side EAGAIN: %v", err)
	}
	want := [][]byte{{0x10}, {0x20}, {0x30}}
	if len(packets) != len(want) {
		t.Fatalf("drain packets = %d, want %d", len(packets), len(want))
	}
	for i := range want {
		if !bytes.Equal(packets[i], want[i]) {
			t.Fatalf("drain packet[%d] = %x, want %x", i, packets[i], want[i])
		}
	}
	if sendCalls != 2 {
		t.Fatalf("terminal send calls = %d, want one EAGAIN plus one successful submission", sendCalls)
	}
	if receiveCalls != len(steps) {
		t.Fatalf("receive calls = %d, want all %d scripted results", receiveCalls, len(steps))
	}
	if !enc.drainSent || !enc.drained {
		t.Fatalf("drain state sent/drained = %v/%v, want true/true", enc.drainSent, enc.drained)
	}

	again, err := enc.drainWith(
		func() error { return errors.New("second terminal send") },
		func() ([]byte, error) { return nil, errors.New("second receive") },
	)
	if err != nil {
		t.Fatalf("idempotent second drain: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("idempotent second drain returned %d packets, want 0", len(again))
	}
}

// Mutation caught: assigning encoder drain packets only the newest submitted
// span, or omitting attribution from the second packet of a multi-packet drain.
func TestFFmpegEncoderAttributedScriptedDrainUsesOutstandingUnionForEveryPacket(t *testing.T) {
	enc := &FFmpegEncoder{codecName: "scripted-aac"}
	enc.trackSourceSpan(SourceSpan{Begin: 10, End: 20})
	enc.trackSourceSpan(SourceSpan{Begin: 30, End: 40})

	type receiveStep struct {
		packet []byte
		err    error
	}
	steps := []receiveStep{
		{packet: []byte{0x10}},
		{packet: []byte{0x20}},
		{err: io.EOF},
	}
	receiveCalls := 0
	receive := func() ([]byte, error) {
		step := steps[receiveCalls]
		receiveCalls++
		return step.packet, step.err
	}

	packets, err := enc.drainAttributedWith(func() error { return nil }, receive)
	if err != nil {
		t.Fatalf("attributed scripted drain: %v", err)
	}
	wantSpan := SourceSpan{Begin: 10, End: 40}
	if len(packets) != 2 {
		t.Fatalf("attributed drain packets = %d, want 2", len(packets))
	}
	for i, packet := range packets {
		if packet.SourceSpan != wantSpan {
			t.Fatalf("attributed drain packet %d span = %+v, want outstanding union %+v", i, packet.SourceSpan, wantSpan)
		}
		if len(packet.Payload) != 1 || packet.Payload[0] != byte((i+1)*0x10) {
			t.Fatalf("attributed drain packet %d payload = %x", i, packet.Payload)
		}
	}
}

// Mutation caught: attributing an immediate encoder packet to only the newest
// submission after an older accepted submission produced no packet.
func TestFFmpegEncoderAttributedDelayedImmediateUsesOutstandingUnion(t *testing.T) {
	enc := &FFmpegEncoder{codecName: "scripted-aac"}
	enc.trackSourceSpan(SourceSpan{Begin: 10, End: 20})

	delayed, err := enc.attributeImmediatePackets(nil)
	if err != nil {
		t.Fatalf("attribute accepted zero-output submission: %v", err)
	}
	if len(delayed) != 0 {
		t.Fatalf("accepted zero-output submission returned %d packets, want 0", len(delayed))
	}

	enc.trackSourceSpan(SourceSpan{Begin: 30, End: 40})
	payload := []byte{0x12, 0x34}
	packets, err := enc.attributeImmediatePackets([][]byte{payload})
	if err != nil {
		t.Fatalf("attribute delayed immediate packet: %v", err)
	}
	if len(packets) != 1 {
		t.Fatalf("delayed immediate packets = %d, want 1", len(packets))
	}
	wantSpan := SourceSpan{Begin: 10, End: 40}
	if packets[0].SourceSpan != wantSpan {
		t.Fatalf("delayed immediate span = %+v, want outstanding union %+v", packets[0].SourceSpan, wantSpan)
	}
	if len(packets[0].Payload) != len(payload) || &packets[0].Payload[0] != &payload[0] {
		t.Fatal("delayed immediate attribution copied or changed the scripted payload")
	}
}

// Mutation caught: changing packet bytes/order/idempotency in the attributed
// path, or assigning an immediate packet an invalid/unrelated interval.
func TestFFmpegEncoderAttributedMatchesLegacyMediaAndDrainsOnce(t *testing.T) {
	legacy := NewFFmpegEncoder("aac", 48000, 2)
	defer legacy.Close()
	attributed := NewFFmpegEncoder("aac", 48000, 2)
	defer attributed.Close()

	frameSize := legacy.FrameSize()
	if frameSize <= 0 || attributed.FrameSize() != frameSize {
		t.Fatalf("AAC frame sizes legacy/attributed = %d/%d", frameSize, attributed.FrameSize())
	}
	for frame := 0; frame < 3; frame++ {
		pcm := &PCMFrame{
			Samples:    make([]int16, frameSize*legacy.Channels()),
			SampleRate: legacy.SampleRate(),
			Channels:   legacy.Channels(),
		}
		for i := range pcm.Samples {
			pcm.Samples[i] = int16(((i+frame*31)%257 - 128) * 128)
		}
		span := SourceSpan{Begin: int64(100 + frame), End: int64(101 + frame)}
		legacyPayload, err := legacy.Encode(pcm)
		if err != nil {
			t.Fatalf("legacy encode frame %d: %v", frame, err)
		}
		packets, err := attributed.EncodeAttributed(pcm, span)
		if err != nil {
			t.Fatalf("attributed encode frame %d: %v", frame, err)
		}
		if len(packets) != 1 {
			t.Fatalf("attributed encode frame %d packets = %d, want 1", frame, len(packets))
		}
		if !bytes.Equal(packets[0].Payload, legacyPayload) {
			t.Fatalf("attributed encode frame %d payload differs from legacy", frame)
		}
		wantSpan := SourceSpan{Begin: 100, End: span.End}
		if packets[0].SourceSpan != wantSpan || !packets[0].SourceSpan.Valid() {
			t.Fatalf("attributed encode frame %d span = %+v, want conservative union %+v", frame, packets[0].SourceSpan, wantSpan)
		}
	}

	legacyTail, err := legacy.Drain()
	if err != nil {
		t.Fatalf("legacy drain: %v", err)
	}
	attributedTail, err := attributed.DrainAttributed()
	if err != nil {
		t.Fatalf("attributed drain: %v", err)
	}
	if len(attributedTail) != len(legacyTail) {
		t.Fatalf("attributed/legacy drain packet counts = %d/%d", len(attributedTail), len(legacyTail))
	}
	wantTailSpan := SourceSpan{Begin: 100, End: 103}
	for i := range legacyTail {
		if !bytes.Equal(attributedTail[i].Payload, legacyTail[i]) {
			t.Fatalf("attributed drain packet %d differs from legacy", i)
		}
		if attributedTail[i].SourceSpan != wantTailSpan {
			t.Fatalf("attributed drain packet %d span = %+v, want %+v", i, attributedTail[i].SourceSpan, wantTailSpan)
		}
	}
	again, err := attributed.DrainAttributed()
	if err != nil {
		t.Fatalf("second attributed drain: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second attributed drain returned %d packets, want 0", len(again))
	}
}
