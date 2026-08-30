package portalloc

import (
	"net"
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

func TestPairAllocatorsStartAtFirstEvenPortWhenMinimumIsOdd(t *testing.T) {
	pa, err := New(10001, 10004)
	if err != nil {
		t.Fatal(err)
	}
	rtpPort, rtcpPort, err := pa.AllocatePair()
	if err != nil {
		t.Fatal(err)
	}
	if rtpPort != 10002 || rtcpPort != 10003 {
		t.Fatalf("allocated pair = %d/%d, want 10002/10003", rtpPort, rtcpPort)
	}

	start, occupiedRTP, occupiedRTCP := reserveFirstOfTwoUDPPairs(t)
	defer occupiedRTP.Close()
	defer occupiedRTCP.Close()
	boundAllocator, err := New(start+1, start+4)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := boundAllocator.AllocateBoundUDPPair("udp4", net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	defer pair.RTPConn.Close()
	defer pair.RTCPConn.Close()
	if pair.RTPPort != start+2 || pair.RTCPPort != start+3 {
		t.Fatalf("bound pair = %d/%d, want %d/%d", pair.RTPPort, pair.RTCPPort, start+2, start+3)
	}
}

func TestAllocateBoundUDPPairSkipsPortsOccupiedOutsideAllocator(t *testing.T) {
	start, occupiedRTP, occupiedRTCP := reserveFirstOfTwoUDPPairs(t)
	defer occupiedRTP.Close()
	defer occupiedRTCP.Close()

	pa, err := New(start, start+3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pair, err := pa.AllocateBoundUDPPair("udp4", net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatalf("AllocateBoundUDPPair: %v", err)
	}
	defer pair.RTPConn.Close()
	defer pair.RTCPConn.Close()
	if pair.RTPPort != start+2 || pair.RTCPPort != start+3 {
		t.Fatalf("bound pair = %d/%d, want unoccupied pair %d/%d", pair.RTPPort, pair.RTCPPort, start+2, start+3)
	}
	if got := pair.RTPConn.LocalAddr().(*net.UDPAddr).Port; got != pair.RTPPort {
		t.Fatalf("RTP socket port = %d, want %d", got, pair.RTPPort)
	}
	if got := pair.RTCPConn.LocalAddr().(*net.UDPAddr).Port; got != pair.RTCPPort {
		t.Fatalf("RTCP socket port = %d, want %d", got, pair.RTCPPort)
	}
}

func reserveFirstOfTwoUDPPairs(t *testing.T) (int, *net.UDPConn, *net.UDPConn) {
	t.Helper()
	loopback := net.ParseIP("127.0.0.1")
	for attempt := 0; attempt < 128; attempt++ {
		probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback})
		if err != nil {
			t.Fatalf("probe UDP range: %v", err)
		}
		start := probe.LocalAddr().(*net.UDPAddr).Port
		_ = probe.Close()
		if start%2 != 0 {
			start--
		}
		if start < 1024 || start+3 > 65535 {
			continue
		}

		conns := make([]*net.UDPConn, 0, 4)
		for port := start; port <= start+3; port++ {
			conn, listenErr := net.ListenUDP("udp4", &net.UDPAddr{IP: loopback, Port: port})
			if listenErr != nil {
				for _, opened := range conns {
					_ = opened.Close()
				}
				conns = nil
				break
			}
			conns = append(conns, conn)
		}
		if len(conns) != 4 {
			continue
		}
		_ = conns[2].Close()
		_ = conns[3].Close()
		return start, conns[0], conns[1]
	}
	t.Fatal("could not reserve two consecutive UDP pairs")
	return 0, nil, nil
}

func TestFreeOutOfRange(t *testing.T) {
	pa, _ := New(10000, 10010)
	// Should not panic
	pa.Free(9999, 10011, 0, 99999)
}
