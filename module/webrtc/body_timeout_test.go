package webrtc

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestSignalingBodyTimeoutReleasesConnectionSlot(t *testing.T) {
	m, server := newTestModule(t)
	if got := m.httpSrv.ReadTimeout; got != 10*time.Second {
		t.Fatalf("signaling ReadTimeout=%s want=10s", got)
	}
	conn, err := net.Dial("tcp", m.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if deadlineErr := conn.SetDeadline(time.Now().Add(13 * time.Second)); deadlineErr != nil {
		t.Fatal(deadlineErr)
	}
	if _, writeErr := fmt.Fprint(conn, "POST /webrtc/whip/slow-body HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/sdp\r\nContent-Length: 100\r\n\r\nv="); writeErr != nil {
		t.Fatal(writeErr)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read timeout response: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("slow body status=%d want=408", response.StatusCode)
	}
	if got := server.ConnectionCount(); got != 0 {
		t.Fatalf("slow body retains %d connection slots", got)
	}
}
