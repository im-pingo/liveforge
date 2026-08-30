package audiocodec

import (
	"testing"
	"unsafe"
)

// Mutation caught: accepting a default/reversed interval or allowing Union to
// bless one invalid operand as an unrelated valid span.
func TestSourceSpanValidationAndConservativeUnion(t *testing.T) {
	tests := []struct {
		name string
		span SourceSpan
		want bool
	}{
		{name: "default", span: SourceSpan{}, want: false},
		{name: "empty", span: SourceSpan{Begin: 7, End: 7}, want: false},
		{name: "reversed", span: SourceSpan{Begin: 8, End: 7}, want: false},
		{name: "negative valid", span: SourceSpan{Begin: -2, End: -1}, want: true},
		{name: "valid", span: SourceSpan{Begin: 7, End: 8}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.span.Valid(); got != tt.want {
				t.Fatalf("SourceSpan.Valid() = %v, want %v", got, tt.want)
			}
		})
	}

	left := SourceSpan{Begin: 10, End: 20}
	right := SourceSpan{Begin: 18, End: 30}
	if got := left.Union(right); got != (SourceSpan{Begin: 10, End: 30}) {
		t.Fatalf("overlapping union = %+v, want [10,30)", got)
	}
	if got := right.Union(left); got != (SourceSpan{Begin: 10, End: 30}) {
		t.Fatalf("reverse-order union = %+v, want [10,30)", got)
	}
	if got := left.Union(SourceSpan{}); got.Valid() {
		t.Fatalf("union with default span = %+v, want invalid", got)
	}
	if got := (SourceSpan{}).Union(right); got.Valid() {
		t.Fatalf("default span union = %+v, want invalid", got)
	}
}

// Mutation caught: clearing pending resampler spans on a zero-output call or
// attributing the later output to only the newest input span.
func TestSourceSpanQueueRetainsZeroOutputContributors(t *testing.T) {
	var queue sourceSpanQueue
	first := SourceSpan{Begin: 100, End: 101}
	second := SourceSpan{Begin: 200, End: 201}
	if !queue.append(8, first) {
		t.Fatal("append first source span failed")
	}
	queue.retainTail(8) // measured zero-output state retains all input samples
	if got := queue.span(); got != first {
		t.Fatalf("zero-output pending span = %+v, want %+v", got, first)
	}
	if !queue.append(8, second) {
		t.Fatal("append second source span failed")
	}
	if got := queue.span(); got != (SourceSpan{Begin: 100, End: 201}) {
		t.Fatalf("later output contributors = %+v, want union [100,201)", got)
	}
}

// Mutation caught: retaining the first source span forever instead of aging
// segments using the measured post-conversion input-sample tail.
func TestSourceSpanQueueAgesOutOldSpanUsingMeasuredTail(t *testing.T) {
	var queue sourceSpanQueue
	first := SourceSpan{Begin: 100, End: 101}
	second := SourceSpan{Begin: 200, End: 201}
	queue.append(8, first)
	queue.append(8, second)

	queue.retainTail(12)
	if got := queue.span(); got != (SourceSpan{Begin: 100, End: 201}) {
		t.Fatalf("partially retained old contributor = %+v, want union [100,201)", got)
	}
	queue.retainTail(4)
	if got := queue.span(); got != second {
		t.Fatalf("measured newest tail span = %+v, want old span aged out and %+v retained", got, second)
	}
	queue.retainTail(0)
	if got := queue.span(); got.Valid() {
		t.Fatalf("empty measured tail span = %+v, want invalid", got)
	}
}

// Mutation caught: rounding an exact fractional resampler delay down or to
// nearest input samples and aging the older span one sample too early.
func TestSourceSpanQueueCeilsFractionalRetainedDelayBeforeAging(t *testing.T) {
	var queue sourceSpanQueue
	first := SourceSpan{Begin: 100, End: 101}
	second := SourceSpan{Begin: 200, End: 201}
	queue.append(1, first)
	queue.append(1, second)

	// LCM(32000, 48000) is 96000: one input sample is three exact
	// delay ticks, so four ticks retain one whole sample plus a fraction.
	retained := ceilRetainedInputSamples(4, 96000, 32000)
	if retained != 2 {
		t.Fatalf("fractional retained samples = %d, want conservative ceil 2", retained)
	}
	queue.retainTail(retained)
	if got := queue.span(); got != (SourceSpan{Begin: 100, End: 201}) {
		t.Fatalf("fractional tail span = %+v, want older contributor retained", got)
	}

	retained = ceilRetainedInputSamples(3, 96000, 32000)
	if retained != 1 {
		t.Fatalf("integral retained samples = %d, want 1", retained)
	}
	queue.retainTail(retained)
	if got := queue.span(); got != second {
		t.Fatalf("integral tail span = %+v, want older contributor aged out and %+v retained", got, second)
	}
}

// Mutation caught: copying an already Go-owned compressed payload merely to
// attach by-value source metadata.
func TestAttributePacketsPreservesPayloadBacking(t *testing.T) {
	first := []byte{0x10, 0x11, 0x12}
	second := []byte{0x20, 0x21}
	span := SourceSpan{Begin: 5, End: 9}

	packets, err := attributePackets([][]byte{first, second}, span)
	if err != nil {
		t.Fatalf("attribute packets: %v", err)
	}
	if len(packets) != 2 {
		t.Fatalf("attributed packets = %d, want 2", len(packets))
	}
	if packets[0].SourceSpan != span || packets[1].SourceSpan != span {
		t.Fatalf("packet spans = %+v/%+v, want %+v", packets[0].SourceSpan, packets[1].SourceSpan, span)
	}
	if unsafe.SliceData(packets[0].Payload) != unsafe.SliceData(first) {
		t.Fatal("first attributed payload does not share its input backing array")
	}
	if unsafe.SliceData(packets[1].Payload) != unsafe.SliceData(second) {
		t.Fatal("second attributed payload does not share its input backing array")
	}
}
