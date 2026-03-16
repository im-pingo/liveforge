package rtmp

import (
	"crypto/rand"
	"net"
	"testing"
)

func TestHandshake(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	errCh := make(chan error, 2)

	// Server side
	go func() {
		errCh <- ServerHandshake(serverConn)
	}()

	// Client side
	go func() {
		errCh <- clientHandshake(clientConn)
	}()

	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("handshake error: %v", err)
		}
	}
}

// clientHandshake simulates an RTMP client handshake.
func clientHandshake(conn net.Conn) error {
	// Send C0 + C1
	c0c1 := make([]byte, 1+1536)
	c0c1[0] = 3 // version
	rand.Read(c0c1[1:])

	if _, err := conn.Write(c0c1); err != nil {
		return err
	}

	// Read S0 + S1 + S2
	s0s1s2 := make([]byte, 1+1536+1536)
	if _, err := readFull(conn, s0s1s2); err != nil {
		return err
	}

	// Send C2 (echo of S1)
	c2 := s0s1s2[1 : 1+1536]
	if _, err := conn.Write(c2); err != nil {
		return err
	}

	return nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		nn, err := conn.Read(buf[n:])
		if err != nil {
			return n + nn, err
		}
		n += nn
	}
	return n, nil
}
