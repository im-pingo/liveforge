package fmp4

import (
	"encoding/binary"
	"io"
)

// Box types as [4]byte arrays.
var (
	BoxFtyp = [4]byte{'f', 't', 'y', 'p'}
	BoxMoov = [4]byte{'m', 'o', 'o', 'v'}
	BoxMvhd = [4]byte{'m', 'v', 'h', 'd'}
	BoxTrak = [4]byte{'t', 'r', 'a', 'k'}
	BoxTkhd = [4]byte{'t', 'k', 'h', 'd'}
	BoxMdia = [4]byte{'m', 'd', 'i', 'a'}
	BoxMdhd = [4]byte{'m', 'd', 'h', 'd'}
	BoxHdlr = [4]byte{'h', 'd', 'l', 'r'}
	BoxMinf = [4]byte{'m', 'i', 'n', 'f'}
	BoxVmhd = [4]byte{'v', 'm', 'h', 'd'}
	BoxSmhd = [4]byte{'s', 'm', 'h', 'd'}
	BoxDinf = [4]byte{'d', 'i', 'n', 'f'}
	BoxDref = [4]byte{'d', 'r', 'e', 'f'}
	BoxUrl  = [4]byte{'u', 'r', 'l', ' '}
	BoxStbl = [4]byte{'s', 't', 'b', 'l'}
	BoxStsd = [4]byte{'s', 't', 's', 'd'}
	BoxStts = [4]byte{'s', 't', 't', 's'}
	BoxStsc = [4]byte{'s', 't', 's', 'c'}
	BoxStsz = [4]byte{'s', 't', 's', 'z'}
	BoxStco = [4]byte{'s', 't', 'c', 'o'}
	BoxMvex = [4]byte{'m', 'v', 'e', 'x'}
	BoxTrex = [4]byte{'t', 'r', 'e', 'x'}
	BoxMoof = [4]byte{'m', 'o', 'o', 'f'}
	BoxMfhd = [4]byte{'m', 'f', 'h', 'd'}
	BoxTraf = [4]byte{'t', 'r', 'a', 'f'}
	BoxTfhd = [4]byte{'t', 'f', 'h', 'd'}
	BoxTfdt = [4]byte{'t', 'f', 'd', 't'}
	BoxTrun = [4]byte{'t', 'r', 'u', 'n'}
	BoxMdat = [4]byte{'m', 'd', 'a', 't'}

	// Codec-specific sample entry boxes
	BoxAvc1 = [4]byte{'a', 'v', 'c', '1'}
	BoxAvcC = [4]byte{'a', 'v', 'c', 'C'}
	BoxHev1 = [4]byte{'h', 'e', 'v', '1'}
	BoxHvcC = [4]byte{'h', 'v', 'c', 'C'}
	BoxAv01 = [4]byte{'a', 'v', '0', '1'}
	BoxAv1C = [4]byte{'a', 'v', '1', 'C'}
	BoxVp08 = [4]byte{'v', 'p', '0', '8'}
	BoxVpcC = [4]byte{'v', 'p', 'c', 'C'}
	BoxVp09 = [4]byte{'v', 'p', '0', '9'}
	BoxMp4a = [4]byte{'m', 'p', '4', 'a'}
	BoxEsds = [4]byte{'e', 's', 'd', 's'}
	BoxMp3  = [4]byte{'.', 'm', 'p', '3'}
	BoxOpus = [4]byte{'O', 'p', 'u', 's'}
	BoxDOps = [4]byte{'d', 'O', 'p', 's'}
)

// WriteBox writes a standard box: [size(4)][type(4)][payload].
func WriteBox(w io.Writer, boxType [4]byte, payload []byte) error {
	size := uint32(8 + len(payload))
	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header[0:4], size)
	copy(header[4:8], boxType[:])
	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

// WriteFullBox writes a full box: [size(4)][type(4)][version(1)][flags(3)][payload].
func WriteFullBox(w io.Writer, boxType [4]byte, version byte, flags uint32, payload []byte) error {
	size := uint32(12 + len(payload))
	header := make([]byte, 12)
	binary.BigEndian.PutUint32(header[0:4], size)
	copy(header[4:8], boxType[:])
	header[8] = version
	header[9] = byte(flags >> 16)
	header[10] = byte(flags >> 8)
	header[11] = byte(flags)
	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

// BoxSize returns 8 + len(payload) for a standard box.
func BoxSize(payload []byte) uint32 {
	return uint32(8 + len(payload))
}

// FullBoxSize returns 12 + len(payload) for a full box.
func FullBoxSize(payload []byte) uint32 {
	return uint32(12 + len(payload))
}
