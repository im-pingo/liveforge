package rtmp

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
)

const handshakeSize = 1536

// ServerHandshake performs the RTMP server-side handshake.
// Simple handshake (version 3, no digest/encryption).
func ServerHandshake(conn net.Conn) error {
	// Read C0 (1 byte) + C1 (1536 bytes)
	c0c1 := make([]byte, 1+handshakeSize)
	if _, err := io.ReadFull(conn, c0c1); err != nil {
		return fmt.Errorf("read C0C1: %w", err)
	}

	version := c0c1[0]
	if version != 3 {
		return fmt.Errorf("unsupported RTMP version: %d", version)
	}

	c1 := c0c1[1:]

	// Build S0 + S1 + S2
	s0s1s2 := make([]byte, 1+handshakeSize+handshakeSize)
	s0s1s2[0] = 3 // version

	// S1: random bytes
	s1 := s0s1s2[1 : 1+handshakeSize]
	rand.Read(s1)

	// S2: echo of C1
	s2 := s0s1s2[1+handshakeSize:]
	copy(s2, c1)

	if _, err := conn.Write(s0s1s2); err != nil {
		return fmt.Errorf("write S0S1S2: %w", err)
	}

	// Read C2 (1536 bytes)
	c2 := make([]byte, handshakeSize)
	if _, err := io.ReadFull(conn, c2); err != nil {
		return fmt.Errorf("read C2: %w", err)
	}

	return nil
}
