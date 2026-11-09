package sparse

import (
	"encoding/binary"
	"math/bits"
)

// blake2s is a minimal in-package BLAKE2s implementation. The Go standard
// library does not ship BLAKE2 and golang.org/x/crypto/blake2s does not expose
// the personalized digest that rag_mcp.sparse uses (hashlib.blake2s with
// digest_size=4 and person=b"rag-bm25"). Only the unkeyed, personalized
// variant is implemented here.

var blake2sIV = [8]uint32{
	0x6A09E667, 0xBB67AE85, 0x3C6EF372, 0xA54FF53A,
	0x510E527F, 0x9B05688C, 0x1F83D9AB, 0x5BE0CD19,
}

var blake2sSigma = [10][16]uint8{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	{14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3},
	{11, 8, 12, 0, 5, 2, 15, 13, 10, 14, 3, 6, 7, 1, 9, 4},
	{7, 9, 3, 1, 13, 12, 11, 14, 2, 6, 5, 10, 4, 0, 15, 8},
	{9, 0, 5, 7, 2, 4, 10, 15, 14, 1, 11, 12, 6, 8, 3, 13},
	{2, 12, 6, 10, 0, 11, 8, 3, 4, 13, 7, 5, 15, 14, 1, 9},
	{12, 5, 1, 15, 14, 13, 4, 10, 0, 7, 6, 3, 9, 2, 8, 11},
	{13, 11, 7, 14, 12, 1, 3, 9, 5, 0, 15, 4, 8, 6, 2, 10},
	{6, 15, 14, 9, 11, 3, 0, 8, 12, 2, 13, 7, 1, 4, 10, 5},
	{10, 2, 8, 4, 7, 6, 1, 5, 15, 11, 9, 14, 3, 12, 13, 0},
}

func blake2sCompress(h *[8]uint32, block []byte, counter uint64, last bool) {
	var m [16]uint32
	for i := 0; i < 16; i++ {
		m[i] = binary.LittleEndian.Uint32(block[i*4:])
	}
	var v [16]uint32
	copy(v[:8], h[:])
	copy(v[8:], blake2sIV[:])
	v[12] ^= uint32(counter)
	v[13] ^= uint32(counter >> 32)
	if last {
		v[14] = ^v[14]
	}
	for round := 0; round < 10; round++ {
		s := &blake2sSigma[round]
		blake2sMix(&v, 0, 4, 8, 12, m[s[0]], m[s[1]])
		blake2sMix(&v, 1, 5, 9, 13, m[s[2]], m[s[3]])
		blake2sMix(&v, 2, 6, 10, 14, m[s[4]], m[s[5]])
		blake2sMix(&v, 3, 7, 11, 15, m[s[6]], m[s[7]])
		blake2sMix(&v, 0, 5, 10, 15, m[s[8]], m[s[9]])
		blake2sMix(&v, 1, 6, 11, 12, m[s[10]], m[s[11]])
		blake2sMix(&v, 2, 7, 8, 13, m[s[12]], m[s[13]])
		blake2sMix(&v, 3, 4, 9, 14, m[s[14]], m[s[15]])
	}
	for i := 0; i < 8; i++ {
		h[i] ^= v[i] ^ v[i+8]
	}
}

func blake2sMix(v *[16]uint32, a, b, c, d int, x, y uint32) {
	v[a] = v[a] + v[b] + x
	v[d] = bits.RotateLeft32(v[d]^v[a], -16)
	v[c] = v[c] + v[d]
	v[b] = bits.RotateLeft32(v[b]^v[c], -12)
	v[a] = v[a] + v[b] + y
	v[d] = bits.RotateLeft32(v[d]^v[a], -8)
	v[c] = v[c] + v[d]
	v[b] = bits.RotateLeft32(v[b]^v[c], -7)
}

// blake2sDigest hashes data with an unkeyed BLAKE2s parameterized by output
// size and the 8-byte personalization field. The byte layout matches Python's
// hashlib.blake2s(data, digest_size=size, person=person).digest().
func blake2sDigest(data []byte, size int, person [8]byte) []byte {
	h := blake2sIV
	// Parameter block words: fanout=1, depth=1 live in byte 2/3 of word 0.
	h[0] ^= 0x01010000 ^ uint32(size)
	h[6] ^= binary.LittleEndian.Uint32(person[0:4])
	h[7] ^= binary.LittleEndian.Uint32(person[4:8])

	counter := uint64(0)
	for len(data) > 64 {
		counter += 64
		blake2sCompress(&h, data[:64], counter, false)
		data = data[64:]
	}
	var block [64]byte
	copy(block[:], data)
	counter += uint64(len(data))
	blake2sCompress(&h, block[:], counter, true)

	var out [32]byte
	for i := 0; i < 8; i++ {
		binary.LittleEndian.PutUint32(out[i*4:], h[i])
	}
	return out[:size]
}
