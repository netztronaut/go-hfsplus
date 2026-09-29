package lzfse

import (
	"encoding/binary"
	"errors"
)

// Limits and alphabet sizes of FSE-compressed blocks.
const (
	matchesPerBlock  = 10000
	literalsPerBlock = 4 * matchesPerBlock

	lSymbols   = 20
	mSymbols   = 20
	dSymbols   = 64
	litSymbols = 256

	lStates   = 64
	mStates   = 64
	dStates   = 256
	litStates = 1024

	// Offsets of the four frequency tables within header.freq, in the
	// order they are stored.
	lFreqOff   = 0
	mFreqOff   = lFreqOff + lSymbols
	dFreqOff   = mFreqOff + mSymbols
	litFreqOff = dFreqOff + dSymbols
	nFreqs     = litFreqOff + litSymbols

	// headerV1Size is the size of the v1 block header: seven uint32 counts,
	// literal_bits, four literal states, lmd_bits, three L/M/D states and
	// the 360 uint16 frequencies (770 bytes), padded to a multiple of four
	// as the reference implementation's C struct is.
	headerV1Size = 772

	// headerV2FixedSize is the part of a v2 header before its variable-length
	// frequency tables: magic, n_raw_bytes and three packed uint64 fields.
	headerV2FixedSize = 32
)

// L, M and D values are coded as a symbol plus a number of extra bits: the
// value is base[symbol] + the extra bits read as an unsigned integer. The
// bases are therefore the running sums of 1<<extra.
var (
	lExtraBits = [lSymbols]uint8{
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 3, 5, 8,
	}
	lBaseValue = [lSymbols]int32{
		0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 20, 28, 60,
	}
	mExtraBits = [mSymbols]uint8{
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 3, 5, 8, 11,
	}
	mBaseValue = [mSymbols]int32{
		0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 24, 56, 312,
	}
	dExtraBits = [dSymbols]uint8{
		0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3,
		4, 4, 4, 4, 5, 5, 5, 5, 6, 6, 6, 6, 7, 7, 7, 7,
		8, 8, 8, 8, 9, 9, 9, 9, 10, 10, 10, 10, 11, 11, 11, 11,
		12, 12, 12, 12, 13, 13, 13, 13, 14, 14, 14, 14, 15, 15, 15, 15,
	}
	dBaseValue = [dSymbols]int32{
		0, 1, 2, 3, 4, 6, 8, 10,
		12, 16, 20, 24, 28, 36, 44, 52,
		60, 76, 92, 108, 124, 156, 188, 220,
		252, 316, 380, 444, 508, 636, 764, 892,
		1020, 1276, 1532, 1788, 2044, 2556, 3068, 3580,
		4092, 5116, 6140, 7164, 8188, 10236, 12284, 14332,
		16380, 20476, 24572, 28668, 32764, 40956, 49148, 57340,
		65532, 81916, 98300, 114684, 131068, 163836, 196604, 229372,
	}
)

// header is a compressed block header in its v1 (unpacked) form; v2
// headers are unpacked into it.
type header struct {
	nRawBytes            uint32
	nLiterals            uint32
	nMatches             uint32
	nLiteralPayloadBytes uint32
	nLMDPayloadBytes     uint32
	literalBits          int32 // bits in the literal stream's last byte, minus 8
	lmdBits              int32 // likewise for the L/M/D stream
	literalState         [4]uint16
	lState               uint16
	mState               uint16
	dState               uint16
	freq                 [nFreqs]uint16
	size                 int // header length in bytes; the payload follows
}

var (
	errShortHeader  = errors.New("header truncated")
	errBadFreqTable = errors.New("malformed frequency table")
	errLimits       = errors.New("header field out of range")
)

// parseV1 reads a "bvx1" header from the start of b.
func (h *header) parseV1(b []byte) error {
	if len(b) < headerV1Size {
		return errShortHeader
	}
	le := binary.LittleEndian
	h.nRawBytes = le.Uint32(b[4:])
	// b[8:12] is n_payload_bytes, the sum of the two payload sizes below;
	// like the reference decoder, we use the two sizes and ignore it.
	h.nLiterals = le.Uint32(b[12:])
	h.nMatches = le.Uint32(b[16:])
	h.nLiteralPayloadBytes = le.Uint32(b[20:])
	h.nLMDPayloadBytes = le.Uint32(b[24:])
	h.literalBits = int32(le.Uint32(b[28:]))
	for i := range h.literalState {
		h.literalState[i] = le.Uint16(b[32+2*i:])
	}
	h.lmdBits = int32(le.Uint32(b[40:]))
	h.lState = le.Uint16(b[44:])
	h.mState = le.Uint16(b[46:])
	h.dState = le.Uint16(b[48:])
	for i := range h.freq {
		h.freq[i] = le.Uint16(b[50+2*i:])
	}
	h.size = headerV1Size
	return h.validate()
}

// field extracts n bits of v starting at bit off.
func field(v uint64, off, n uint) uint32 {
	return uint32(v >> off & (1<<n - 1))
}

// parseV2 reads a "bvx2" header from the start of b. Its three packed
// 64-bit words hold (bit offset: width):
//
//	word 0: n_literals 0:20, n_literal_payload_bytes 20:20, n_matches 40:20,
//	        literal_bits+7 60:3
//	word 1: literal_state[0..3] 0:10 10:10 20:10 30:10,
//	        n_lmd_payload_bytes 40:20, lmd_bits+7 60:3
//	word 2: header size 0:32, l_state 32:10, m_state 42:10, d_state 52:10
//
// The frequency tables follow as a bit stream of variable-length codes (see
// decodeFreq) that must end, padded to a byte, exactly at the header size.
// A header size of 32 means the tables are omitted and all zero.
func (h *header) parseV2(b []byte) error {
	if len(b) < headerV2FixedSize {
		return errShortHeader
	}
	le := binary.LittleEndian
	h.nRawBytes = le.Uint32(b[4:])
	v0, v1, v2 := le.Uint64(b[8:]), le.Uint64(b[16:]), le.Uint64(b[24:])

	h.nLiterals = field(v0, 0, 20)
	h.nLiteralPayloadBytes = field(v0, 20, 20)
	h.nMatches = field(v0, 40, 20)
	h.literalBits = int32(field(v0, 60, 3)) - 7
	for i := range h.literalState {
		h.literalState[i] = uint16(field(v1, uint(10*i), 10))
	}
	h.nLMDPayloadBytes = field(v1, 40, 20)
	h.lmdBits = int32(field(v1, 60, 3)) - 7
	size := uint64(field(v2, 0, 32))
	h.lState = uint16(field(v2, 32, 10))
	h.mState = uint16(field(v2, 42, 10))
	h.dState = uint16(field(v2, 52, 10))

	if size < headerV2FixedSize {
		return errBadFreqTable
	}
	if size > uint64(len(b)) {
		return errShortHeader
	}
	h.size = int(size)
	h.freq = [nFreqs]uint16{}
	if h.size > headerV2FixedSize {
		tab := b[headerV2FixedSize:h.size]
		var (
			accum uint32
			nbits int
			p     int
		)
		for i := range h.freq {
			// Keep at least 25 bits buffered (more than the longest code)
			// while input remains.
			for p < len(tab) && nbits+8 <= 32 {
				accum |= uint32(tab[p]) << uint(nbits)
				nbits += 8
				p++
			}
			v, n := decodeFreq(accum)
			if n > nbits {
				return errBadFreqTable
			}
			h.freq[i] = v
			accum >>= uint(n)
			nbits -= n
		}
		// The tables must end in the last byte of the header.
		if nbits >= 8 || p != len(tab) {
			return errBadFreqTable
		}
	}
	return h.validate()
}

// validate checks the counts, states and frequency sums against the format's
// limits. The payload sizes are checked against the input in decodeBlock.
func (h *header) validate() error {
	if h.nLiterals > literalsPerBlock || h.nMatches > matchesPerBlock {
		return errLimits
	}
	// Every literal a block stores is copied to its output, except for at
	// most three that pad the count to a multiple of four, and every L/M/D
	// triple produces at least one byte. The reference decoder does not
	// check this, but Apple's encoder cannot violate it, and enforcing it
	// bounds the decoding work by the output size: without it, a 150-byte
	// block could demand 50000 symbol decodes that produce nothing.
	if uint64(h.nLiterals) > uint64(h.nRawBytes)+3 || h.nMatches > h.nRawBytes {
		return errLimits
	}
	for _, s := range h.literalState {
		if s >= litStates {
			return errLimits
		}
	}
	if h.lState >= lStates || h.mState >= mStates || h.dState >= dStates {
		return errLimits
	}
	if !freqSumOK(h.freq[lFreqOff:mFreqOff], lStates) ||
		!freqSumOK(h.freq[mFreqOff:dFreqOff], mStates) ||
		!freqSumOK(h.freq[dFreqOff:litFreqOff], dStates) ||
		!freqSumOK(h.freq[litFreqOff:], litStates) {
		return errBadFreqTable
	}
	return nil
}

// decoder holds the per-block tables and the literal buffer, reused across
// the blocks of one stream.
type decoder struct {
	lit      [litStates]litEntry
	l        [lStates]valueEntry
	m        [mStates]valueEntry
	d        [dStates]valueEntry
	literals [literalsPerBlock]byte
}

var (
	errPayload   = errors.New("payload extends beyond input")
	errBitstream = errors.New("malformed bit stream")
	errLiterals  = errors.New("matches use more literals than decoded")
	errOverrun   = errors.New("block decodes to more bytes than declared")
	errUnderrun  = errors.New("block decodes to fewer bytes than declared")
	errDistance  = errors.New("match distance out of range")
)

// decodeBlock decodes the FSE-compressed block at the start of blk, whose
// header h has been parsed and validated, into dst starting at dst[o]. The
// caller has checked that h.nRawBytes fits in dst[o:]. It returns the bytes
// written and the length of the block in blk.
//
// The payload holds the literal bit stream followed by the L/M/D bit stream.
// Both are read backwards, from their last byte.
func (dec *decoder) decodeBlock(dst []byte, o int, blk []byte, h *header) (written, used int, err error) {
	litEnd := uint64(h.size) + uint64(h.nLiteralPayloadBytes)
	lmdEnd := litEnd + uint64(h.nLMDPayloadBytes)
	if lmdEnd > uint64(len(blk)) {
		return 0, 0, errPayload
	}

	initLitTable(dec.lit[:], h.freq[litFreqOff:])
	initValueTable(dec.l[:], h.freq[lFreqOff:mFreqOff], lExtraBits[:], lBaseValue[:])
	initValueTable(dec.m[:], h.freq[mFreqOff:dFreqOff], mExtraBits[:], mBaseValue[:])
	initValueTable(dec.d[:], h.freq[dFreqOff:litFreqOff], dExtraBits[:], dBaseValue[:])

	// Literals are interleaved over four FSE states and decoded four per
	// refill (at most 4*10 bits), so n_literals is effectively rounded up to
	// a multiple of four; as the limit of 40000 is one, that still fits the
	// buffer.
	var r bitReader
	if !r.init(blk[:litEnd], h.literalBits) {
		return 0, 0, errBitstream
	}
	st := h.literalState
	nLit := int(h.nLiterals)
	lits := dec.literals[:]
	for i := 0; i < nLit; i += 4 {
		if !r.refill() {
			return 0, 0, errBitstream
		}
		lits[i+0] = decodeLit(&st[0], dec.lit[:], &r)
		lits[i+1] = decodeLit(&st[1], dec.lit[:], &r)
		lits[i+2] = decodeLit(&st[2], dec.lit[:], &r)
		lits[i+3] = decodeLit(&st[3], dec.lit[:], &r)
	}

	// L/M/D triples: one refill per triple suffices, as a triple takes at
	// most (6+8) + (6+11) + (8+15) = 54 bits.
	if !r.init(blk[:lmdEnd], h.lmdBits) {
		return 0, 0, errBitstream
	}
	var (
		start = o
		end   = o + int(h.nRawBytes)
		lp    = 0  // next literal to copy
		d     = -1 // current match distance; -1 until the block sets one
	)
	ls, ms, ds := h.lState, h.mState, h.dState
	for range h.nMatches {
		if !r.refill() {
			return o - start, 0, errBitstream
		}
		l := decodeValue(&ls, dec.l[:], &r)
		m := decodeValue(&ms, dec.m[:], &r)
		// A D value of 0 repeats the previous distance.
		if nd := decodeValue(&ds, dec.d[:], &r); nd != 0 {
			d = nd
		}

		if l > nLit-lp {
			return o - start, 0, errLiterals
		}
		if l+m > end-o {
			return o - start, 0, errOverrun
		}
		// History is the whole output so far, including earlier blocks and
		// this triple's literals. Like the reference decoder we check the
		// distance even when m == 0.
		if d < 1 || d > o+l {
			return o - start, 0, errDistance
		}
		o += copy(dst[o:o+l], lits[lp:lp+l])
		lp += l
		copyMatch(dst, o, d, m)
		o += m
	}
	if o != end {
		return o - start, 0, errUnderrun
	}
	return o - start, int(lmdEnd), nil
}

// copyMatch copies m bytes from dst[o-d:] to dst[o:]. The regions may
// overlap (d < m), in which case the last d bytes repeat. The caller
// guarantees 0 < d <= o and o+m <= len(dst).
func copyMatch(dst []byte, o, d, m int) {
	if d >= m {
		copy(dst[o:o+m], dst[o-d:])
		return
	}
	end := o + m
	for o < end {
		n := copy(dst[o:end], dst[o-d:o])
		o += n
		d += n
	}
}
