package lzfse

import (
	"encoding/binary"
	"math/bits"
)

// This file holds the finite-state-entropy (tANS) machinery: the backward
// bit reader and the decoder tables built from a block's symbol frequencies.

// bitReader reads an FSE bit stream. The encoder writes the stream forwards
// while encoding symbols in reverse order, so the decoder starts at the end
// of the payload and moves towards its beginning. Bits are consumed from the
// most significant end of accum.
type bitReader struct {
	buf   []byte // readable bytes; the reader moves from len(buf) towards 0
	pos   int    // bytes buf[pos:] have been loaded into accum
	accum uint64 // the low nbits bits are valid, all higher bits are zero
	nbits int    // number of valid bits in accum
}

// init prepares r to read the stream whose last byte is buf[len(buf)-1].
// extra is the header's "bits" field (literal_bits or lmd_bits): the
// encoder's final partial byte holds 8+extra meaningful bits, extra being
// in -7..0.
//
// buf may extend before the start of the stream (it is the block from its
// magic onwards): refills read whole bytes ahead of need, so a valid stream
// can load up to 8 bytes that precede it without consuming any of their
// bits. Apple's decoder likewise bounds these reads only by the start of its
// input buffer.
func (r *bitReader) init(buf []byte, extra int32) bool {
	r.buf = buf
	n := len(buf)
	if extra != 0 {
		if n < 8 {
			return false
		}
		r.accum = binary.LittleEndian.Uint64(buf[n-8:])
		r.pos = n - 8
		r.nbits = int(extra) + 64
	} else {
		if n < 7 {
			return false
		}
		var b [8]byte
		copy(b[:7], buf[n-7:])
		r.accum = binary.LittleEndian.Uint64(b[:])
		r.pos = n - 7
		r.nbits = 56
	}
	// After the first load there must be 56..63 valid bits, and the bits
	// above them (padding written by the encoder) must be zero.
	if r.nbits < 56 || r.nbits > 63 || r.accum>>uint(r.nbits) != 0 {
		return false
	}
	return true
}

// refill tops accum up to 56..63 valid bits by loading whole bytes. It
// reports false if that would read before the start of buf.
func (r *bitReader) refill() bool {
	n := (63 - r.nbits) >> 3 // bytes to load
	if n > r.pos {
		return false
	}
	var in uint64
	for i := 1; i <= n; i++ {
		in = in<<8 | uint64(r.buf[r.pos-i])
	}
	r.pos -= n
	r.accum = r.accum<<uint(n*8) | in
	r.nbits += n * 8
	return true
}

// pull removes and returns the n most significant valid bits. Callers never
// pull more than the 56 bits a refill guarantees (see the bounds noted at
// the call sites); n is additionally clamped so that a logic error could not
// turn into a negative shift.
func (r *bitReader) pull(n uint) uint64 {
	if int(n) > r.nbits {
		n = uint(r.nbits)
	}
	r.nbits -= int(n)
	v := r.accum >> uint(r.nbits)
	r.accum &= 1<<uint(r.nbits) - 1
	return v
}

// litEntry is one state of the literal decoder: decoding in this state
// yields sym, then the next state is delta plus the next k bits.
type litEntry struct {
	delta int32
	k     uint8
	sym   uint8
}

// valueEntry is one state of an L, M or D decoder. Decoding pulls
// totalBits bits: the high totalBits-valueBits of them are added to delta to
// form the next state, the low valueBits are added to vbase to form the
// value.
type valueEntry struct {
	vbase     int32
	delta     int16
	totalBits uint8
	valueBits uint8
}

// spread calls fn(state, symbol, k, delta) for every state of an FSE table
// with nstates states (a power of two) and the given frequencies, in the
// order the encoder assigns them: symbols in increasing order, each owning
// freq[symbol] consecutive states.
//
// A symbol with frequency f uses k bits for its first j0 states and k-1
// bits for the others, where k is chosen so that nstates <= f<<k < 2*nstates
// and j0 = 2*nstates>>k - f. This is the tANS state-transition rule of the
// reference implementation.
//
// The caller must have checked that the frequencies sum to at most nstates;
// states beyond the sum are left untouched.
func spread(nstates int, freq []uint16, fn func(state, sym int, k uint, delta int)) {
	logN := bits.Len(uint(nstates)) - 1
	state := 0
	for sym, f16 := range freq {
		f := int(f16)
		if f == 0 {
			continue
		}
		k := uint(logN - (bits.Len(uint(f)) - 1))
		j0 := (2*nstates)>>k - f
		for j := 0; j < f; j++ {
			if j < j0 {
				fn(state, sym, k, (f+j)<<k-nstates)
			} else {
				fn(state, sym, k-1, (j-j0)<<(k-1))
			}
			state++
		}
	}
}

// freqSumOK reports whether freq sums to at most nstates, the condition for
// spread to stay within the table.
func freqSumOK(freq []uint16, nstates int) bool {
	sum := 0
	for _, f := range freq {
		sum += int(f)
	}
	return sum <= nstates
}

// initLitTable fills t (whose length is the state count) for the literal
// alphabet. Unassigned states decode as symbol 0 with next state 0.
func initLitTable(t []litEntry, freq []uint16) {
	clear(t)
	spread(len(t), freq, func(state, sym int, k uint, delta int) {
		t[state] = litEntry{delta: int32(delta), k: uint8(k), sym: uint8(sym)}
	})
}

// initValueTable fills t for an L, M or D alphabet whose symbols carry
// extra[sym] extra bits on top of base[sym].
func initValueTable(t []valueEntry, freq []uint16, extra []uint8, base []int32) {
	clear(t)
	spread(len(t), freq, func(state, sym int, k uint, delta int) {
		t[state] = valueEntry{
			vbase:     base[sym],
			delta:     int16(delta),
			totalBits: uint8(k) + extra[sym],
			valueBits: extra[sym],
		}
	})
}

// decodeLit decodes one literal. The state is masked to the table size,
// which is a no-op for tables built by initLitTable (every next state is
// below the state count) and keeps corrupt input from indexing outside t.
func decodeLit(state *uint16, t []litEntry, r *bitReader) byte {
	e := t[int(*state)&(len(t)-1)]
	*state = uint16(e.delta + int32(r.pull(uint(e.k))))
	return e.sym
}

// decodeValue decodes one L, M or D value.
func decodeValue(state *uint16, t []valueEntry, r *bitReader) int {
	e := t[int(*state)&(len(t)-1)]
	v := r.pull(uint(e.totalBits))
	*state = uint16(int(e.delta) + int(v>>e.valueBits))
	return int(e.vbase) + int(v&(1<<e.valueBits-1))
}

// decodeFreq decodes one entry of a v2 header's frequency tables from the
// low bits of b, returning the value and the number of bits it occupies.
// The code is prefix-free, read from the least significant bit:
//
//	   x0  (2 bits)  value 0 or 1 (the x bit)
//	  x01  (3 bits)  value 2 or 3
//	xx011  (5 bits)  value 4..7
//	 0111  followed by 4 bits:  value 8 + those bits  (8 bits in all)
//	 1111  followed by 10 bits: value 24 + those bits (14 bits in all)
func decodeFreq(b uint32) (value uint16, nbits int) {
	switch {
	case b&1 == 0:
		return uint16(b >> 1 & 1), 2
	case b&3 == 1:
		return 2 + uint16(b>>2&1), 3
	case b&7 == 3:
		return 4 + uint16(b>>3&3), 5
	case b&15 == 7:
		return 8 + uint16(b>>4&0xf), 8
	default: // b&15 == 15
		return 24 + uint16(b>>4&0x3ff), 14
	}
}
