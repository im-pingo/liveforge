package mp4

import (
	"encoding/binary"
	"io"
)

func writeBox(w io.Writer, boxType [4]byte, payload []byte) {
	size := uint32(8 + len(payload))
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], size)
	copy(hdr[4:8], boxType[:])
	w.Write(hdr[:])
	w.Write(payload)
}

func writeFullBox(w io.Writer, boxType [4]byte, version byte, flags uint32, payload []byte) {
	size := uint32(12 + len(payload))
	var hdr [12]byte
	binary.BigEndian.PutUint32(hdr[0:4], size)
	copy(hdr[4:8], boxType[:])
	hdr[8] = version
	binary.BigEndian.PutUint32(hdr[8:12], (uint32(version)<<24)|(flags&0x00FFFFFF))
	w.Write(hdr[:])
	w.Write(payload)
}

func putU16(buf []byte, v uint16) {
	binary.BigEndian.PutUint16(buf, v)
}

func putU32(buf []byte, v uint32) {
	binary.BigEndian.PutUint32(buf, v)
}

func putU64(buf []byte, v uint64) {
	binary.BigEndian.PutUint64(buf, v)
}

func boxSize(boxType [4]byte, payload []byte) int {
	return 8 + len(payload)
}
