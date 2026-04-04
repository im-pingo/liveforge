package portalloc

import (
	"testing"
)

func TestNew(t *testing.T) {
	_, err := New(10000, 10010)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = New(0, 100)
	if err == nil {
		t.Fatal("expected error for port 0")
	}

	_, err = New(10010, 10000)
	if err == nil {
		t.Fatal("expected error for reversed range")
	}
}

func TestAllocate(t *testing.T) {
	pa, _ := New(20000, 20002)

	p1, err := pa.Allocate()
	if err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	if p1 < 20000 || p1 > 20002 {
		t.Fatalf("port out of range: %d", p1)
	}

	p2, err := pa.Allocate()
	if err != nil {
		t.Fatalf("second allocate: %v", err)
	}
	if p2 == p1 {
		t.Fatal("duplicate port allocated")
	}

	p3, err := pa.Allocate()
	if err != nil {
		t.Fatalf("third allocate: %v", err)
	}

	_, err = pa.Allocate()
	if err == nil {
		t.Fatal("expected exhaustion error")
	}

	pa.Free(p2)
	p4, err := pa.Allocate()
	if err != nil {
		t.Fatalf("allocate after free: %v", err)
	}
	if p4 != p2 {
		t.Fatalf("expected freed port %d, got %d", p2, p4)
	}
	_ = p3
}

func TestAllocatePair(t *testing.T) {
	pa, _ := New(10000, 10003) // 2 pairs: 10000/10001, 10002/10003

	rtp1, rtcp1, err := pa.AllocatePair()
	if err != nil {
		t.Fatalf("first pair: %v", err)
	}
	if rtp1%2 != 0 {
		t.Fatalf("rtp port not even: %d", rtp1)
	}
	if rtcp1 != rtp1+1 {
		t.Fatalf("rtcp port not rtp+1: rtp=%d rtcp=%d", rtp1, rtcp1)
	}

	rtp2, rtcp2, err := pa.AllocatePair()
	if err != nil {
		t.Fatalf("second pair: %v", err)
	}
	if rtp2 == rtp1 {
		t.Fatal("duplicate pair allocated")
	}

	_, _, err = pa.AllocatePair()
	if err == nil {
		t.Fatal("expected exhaustion error")
	}

	pa.Free(rtp1, rtcp1)
	rtp3, _, err := pa.AllocatePair()
	if err != nil {
		t.Fatalf("pair after free: %v", err)
	}
	if rtp3 != rtp1 {
		t.Fatalf("expected freed pair starting at %d, got %d", rtp1, rtp3)
	}
	_ = rtcp2
}

func TestFreeOutOfRange(t *testing.T) {
	pa, _ := New(10000, 10010)
	// Should not panic
	pa.Free(9999, 10011, 0, 99999)
}
