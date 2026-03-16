package rtmp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

const handshakeSize = 1536

// RTMP handshake keys for HMAC-SHA256 digest verification.
var (
	genuineFPKey = []byte("Genuine Adobe Flash Player 001") // 30 bytes
	genuineFMSKey = []byte("Genuine Adobe Flash Media Server 001") // 36 bytes

	// Full 68-byte server key (36 + 32 random bytes from spec)
	genuineFMSKeyFull = []byte{
		0x47, 0x65, 0x6e, 0x75, 0x69, 0x6e, 0x65, 0x20,
		0x41, 0x64, 0x6f, 0x62, 0x65, 0x20, 0x46, 0x6c,
		0x61, 0x73, 0x68, 0x20, 0x4d, 0x65, 0x64, 0x69,
		0x61, 0x20, 0x53, 0x65, 0x72, 0x76, 0x65, 0x72,
		0x20, 0x30, 0x30, 0x31,
		0xf0, 0xee, 0xc2, 0x4a, 0x80, 0x68, 0xbe, 0xe8,
		0x2e, 0x00, 0xd0, 0xd1, 0x02, 0x9e, 0x7e, 0x57,
		0x6e, 0xec, 0x5d, 0x2d, 0x29, 0x80, 0x6f, 0xab,
		0x93, 0xb8, 0xe6, 0x36, 0xcf, 0xeb, 0x31, 0xae,
	}

	// Full 62-byte client key (30 + 32 random bytes from spec)
	genuineFPKeyFull = []byte{
		0x47, 0x65, 0x6E, 0x75, 0x69, 0x6E, 0x65, 0x20,
		0x41, 0x64, 0x6F, 0x62, 0x65, 0x20, 0x46, 0x6C,
		0x61, 0x73, 0x68, 0x20, 0x50, 0x6C, 0x61, 0x79,
		0x65, 0x72, 0x20, 0x30, 0x30, 0x31,
		0xF0, 0xEE, 0xC2, 0x4A, 0x80, 0x68, 0xBE, 0xE8,
		0x2E, 0x00, 0xD0, 0xD1, 0x02, 0x9E, 0x7E, 0x57,
		0x6E, 0xEC, 0x5D, 0x2D, 0x29, 0x80, 0x6F, 0xAB,
		0x93, 0xB8, 0xE6, 0x36, 0xCF, 0xEB, 0x31, 0xAE,
	}
)

// ServerHandshake performs the RTMP server-side handshake.
// Tries complex handshake first (HMAC-SHA256 digest); falls back to simple if C1 has no digest.
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

	// Try to find C1 digest (complex handshake)
	digestOffset, isComplex := findDigest(c1, genuineFPKey)

	var s1, s2 []byte

	if isComplex {
		s1 = makeS1Complex()
		s2 = makeS2Complex(c1, digestOffset)
	} else {
		// Simple handshake fallback
		s1 = make([]byte, handshakeSize)
		rand.Read(s1)
		s2 = make([]byte, handshakeSize)
		copy(s2, c1)
	}

	// Write S0 + S1 + S2
	s0s1s2 := make([]byte, 0, 1+handshakeSize+handshakeSize)
	s0s1s2 = append(s0s1s2, 3) // S0: version
	s0s1s2 = append(s0s1s2, s1...)
	s0s1s2 = append(s0s1s2, s2...)

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

// findDigest tries to locate the HMAC-SHA256 digest in a C1/S1 packet.
// The digest can be at one of two schema positions.
// Returns the offset of the digest and whether a valid digest was found.
func findDigest(data []byte, key []byte) (int, bool) {
	// Schema 1: digest at offset computed from bytes 8-11
	offset := calcDigestOffset(data[8:12], 12)
	if validateDigest(data, offset, key) {
		return offset, true
	}

	// Schema 0: digest at offset computed from bytes 772-775
	offset = calcDigestOffset(data[772:776], 776)
	if validateDigest(data, offset, key) {
		return offset, true
	}

	return 0, false
}

// calcDigestOffset computes the digest position from 4 bytes.
func calcDigestOffset(b []byte, base int) int {
	offset := int(b[0]) + int(b[1]) + int(b[2]) + int(b[3])
	offset = (offset % 728) + base
	return offset
}

// validateDigest checks if a 32-byte HMAC-SHA256 digest at the given offset is valid.
func validateDigest(data []byte, offset int, key []byte) bool {
	if offset+32 > len(data) {
		return false
	}

	// Compute HMAC over everything except the 32-byte digest
	msg := make([]byte, 0, len(data)-32)
	msg = append(msg, data[:offset]...)
	msg = append(msg, data[offset+32:]...)

	h := hmac.New(sha256.New, key)
	h.Write(msg)
	expected := h.Sum(nil)

	return hmac.Equal(expected, data[offset:offset+32])
}

// makeS1Complex builds an S1 packet with a valid HMAC-SHA256 digest.
func makeS1Complex() []byte {
	s1 := make([]byte, handshakeSize)

	// Timestamp
	binary.BigEndian.PutUint32(s1[0:4], 0)
	// Server version (e.g., 9.0.124.2 = FMS version)
	s1[4] = 9
	s1[5] = 0
	s1[6] = 124
	s1[7] = 2

	// Fill with random data
	rand.Read(s1[8:])

	// Use schema 1: digest offset computed from bytes 8-11
	offset := calcDigestOffset(s1[8:12], 12)

	// Compute digest over everything except the 32-byte digest area
	msg := make([]byte, 0, handshakeSize-32)
	msg = append(msg, s1[:offset]...)
	msg = append(msg, s1[offset+32:]...)

	h := hmac.New(sha256.New, genuineFMSKey)
	h.Write(msg)
	digest := h.Sum(nil)

	copy(s1[offset:], digest)
	return s1
}

// makeS2Complex builds an S2 packet that the client will validate.
// S2 is 1536 bytes: 1504 bytes of random data + 32-byte HMAC digest.
func makeS2Complex(c1 []byte, c1DigestOffset int) []byte {
	s2 := make([]byte, handshakeSize)

	// Fill with random data
	rand.Read(s2[:handshakeSize-32])

	// The S2 digest key is derived from the C1 digest
	c1Digest := c1[c1DigestOffset : c1DigestOffset+32]

	// Create the key: HMAC-SHA256 of C1 digest with the full FMS key
	keyH := hmac.New(sha256.New, genuineFMSKeyFull)
	keyH.Write(c1Digest)
	s2Key := keyH.Sum(nil)

	// Compute the S2 digest: HMAC-SHA256 of the first 1504 bytes with the derived key
	h := hmac.New(sha256.New, s2Key)
	h.Write(s2[:handshakeSize-32])
	digest := h.Sum(nil)

	copy(s2[handshakeSize-32:], digest)
	return s2
}
