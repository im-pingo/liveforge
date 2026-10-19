package rtsp

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestIncompleteRTSPControlBodyReleasesConnection(t *testing.T) {
	m, server := newShutdownTestModule(t, time.Second)
	defer m.Close()
	conn, err := net.Dial("tcp", m.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitForRTSPConnections(t, server, 1)
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "ANNOUNCE rtsp://host/live/stalled RTSP/1.0\r\nContent-Length: 10\r\n\r\nx"); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	_, err = conn.Read(b[:])
	if err == nil {
		t.Fatal("incomplete request produced a response")
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		t.Fatal("server left the incomplete control request open beyond its read budget")
	}
	waitForRTSPConnections(t, server, 0)
}
