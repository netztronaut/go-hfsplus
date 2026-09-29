// Package lzvn decodes raw LZVN streams.
//
// LZVN is Apple's byte-oriented LZ77 variant. It appears as the payload of
// "bvxn" blocks inside LZFSE streams and as the chunks of decmpfs-compressed
// files of types 7 and 8 on HFS+ and APFS. The bitstream layout implemented
// here follows Apple's reference implementation (lzvn_decode_base.c in the
// BSD-licensed lzfse project); the code is an independent implementation.
//
// The decoder is written for untrusted input: every read of src and every
// match copy is bounds-checked, it never writes beyond len(dst), it allocates
// nothing, and it runs in time linear in len(src)+len(dst).
package lzvn

import (
	"errors"
	"fmt"
)

// ErrCorrupt is returned (possibly wrapped) for malformed input.
var ErrCorrupt = errors.New("lzvn: corrupt input")

// ErrDstTooSmall is returned when the decoded stream would exceed len(dst).
var ErrDstTooSmall = errors.New("lzvn: output larger than destination")

// Opcode summary. Each opcode is followed by its operand bytes, then by L
// literal bytes (which are copied first), and then describes a match of M
// bytes at distance D back from the current output position.
//
//	sml_d  LLMMMDDD DDDDDDDD                    L=0..3  M=3..10  D=11 bits
//	med_d  101LLMMM DDDDDDMM DDDDDDDD           L=0..3  M=3..34  D=14 bits
//	lrg_d  LLMMM111 DDDDDDDD DDDDDDDD           L=0..3  M=3..10  D=16 bits
//	pre_d  LLMMM110                             L=0..3  M=3..10  D=previous
//	sml_m  1111MMMM                             L=0     M=1..15  D=previous
//	lrg_m  11110000 MMMMMMMM                    L=0     M=16..271 D=previous
//	sml_l  1110LLLL                             L=1..15 M=0
//	lrg_l  11100000 LLLLLLLL                    L=16..271 M=0
//	nop    0x0e, 0x16
//	eos    0x06 (followed by 7 bytes of zero padding)
//
// Opcodes 0x1e, 0x26, 0x2e, 0x36, 0x3e, 0x70-0x7f and 0xd0-0xdf are
// undefined. The previous distance starts at 0, which is never a valid
// distance, so a stream that begins with pre_d, sml_m or lrg_m is corrupt.
const (
	opEOS = 0x06
)

// Decode decodes one raw LZVN stream (as found in an lzfse "bvxn" block
// payload and in decmpfs type 7/8 chunks) from src into dst.
// It stops at the end-of-stream opcode (0x06) or when src is exhausted, and
// returns the number of bytes written.
//
// Precisely:
//
//   - Decoding proceeds opcode by opcode from src[0]. When the opcode 0x06
//     (end of stream) is read, Decode returns the bytes written so far and a
//     nil error. The 7 padding bytes that normally follow it are neither
//     required nor inspected, and anything after the end-of-stream opcode is
//     ignored.
//   - When src is exhausted exactly at an opcode boundary, Decode also
//     returns the bytes written so far and a nil error. This lets callers
//     that know only the expected output size (decmpfs) decode streams whose
//     trailer is missing; such callers should compare the returned count
//     with the size they expect. In particular, a dst that has become
//     exactly full followed by either the end-of-stream opcode or the end of
//     src is a success.
//   - If src ends inside an opcode, its operands or its literals, the error
//     wraps ErrCorrupt.
//   - Undefined opcodes, a match distance of 0 and a match distance greater
//     than the number of bytes written so far are reported as ErrCorrupt.
//     History is limited to dst itself: dst[0] is the first byte of the
//     stream.
//   - If a literal run or match would write beyond len(dst), the error wraps
//     ErrDstTooSmall. dst is never written beyond its length.
//
// A decmpfs chunk whose first byte is 0x06 holds its data uncompressed after
// that byte; recognising that convention is up to the caller (Decode would
// see an end-of-stream opcode and return 0).
//
// On error the returned count is the number of bytes already written to
// dst; their content must not be trusted.
func Decode(dst, src []byte) (int, error) {
	var (
		s     int // read position in src
		o     int // write position in dst
		dPrev int // previous match distance; 0 means "none yet"
	)
	for s < len(src) {
		op := src[s]
		var (
			opLen int // opcode length including operands
			l     int // literal count
			m     int // match length
			d     int // match distance
		)
		switch {
		case op == opEOS:
			return o, nil
		case op == 0x0e || op == 0x16: // nop
			s++
			continue
		case op == 0xf0: // lrg_m
			if len(src)-s < 2 {
				return o, truncated(s)
			}
			opLen, m, d = 2, int(src[s+1])+16, dPrev
		case op > 0xf0: // sml_m
			opLen, m, d = 1, int(op&0x0f), dPrev
		case op == 0xe0: // lrg_l
			if len(src)-s < 2 {
				return o, truncated(s)
			}
			opLen, l = 2, int(src[s+1])+16
		case op > 0xe0: // sml_l
			opLen, l = 1, int(op&0x0f)
		case op >= 0xd0, op >= 0x70 && op < 0x80:
			return o, fmt.Errorf("%w: undefined opcode 0x%02x at offset %d", ErrCorrupt, op, s)
		case op >= 0xa0 && op < 0xc0: // med_d
			if len(src)-s < 3 {
				return o, truncated(s)
			}
			b1, b2 := int(src[s+1]), int(src[s+2])
			opLen = 3
			l = int(op>>3) & 3
			m = (int(op&7)<<2 | b1&3) + 3
			d = b1>>2 | b2<<6
		default:
			// 0x00-0x6f, 0x80-0x9f, 0xc0-0xcf: the low three bits select the
			// distance form, the high bits hold L and M.
			l = int(op >> 6)
			m = int(op>>3&7) + 3
			switch op & 7 {
			case 7: // lrg_d
				if len(src)-s < 3 {
					return o, truncated(s)
				}
				opLen = 3
				d = int(src[s+1]) | int(src[s+2])<<8
			case 6:
				if op < 0x40 { // 0x1e ... 0x3e (0x06, 0x0e, 0x16 handled above)
					return o, fmt.Errorf("%w: undefined opcode 0x%02x at offset %d", ErrCorrupt, op, s)
				}
				opLen, d = 1, dPrev // pre_d
			default: // sml_d
				if len(src)-s < 2 {
					return o, truncated(s)
				}
				opLen = 2
				d = int(op&7)<<8 | int(src[s+1])
			}
		}

		// Literals follow the opcode and its operands.
		s += opLen
		if l > 0 {
			if len(src)-s < l {
				return o, truncated(s - opLen)
			}
			if len(dst)-o < l {
				return o, fmt.Errorf("%w: literal run at output offset %d", ErrDstTooSmall, o)
			}
			o += copy(dst[o:], src[s:s+l])
			s += l
		}
		if m == 0 {
			continue
		}

		// The match: distances are checked against the bytes written so
		// far, which include the literals just copied.
		if d == 0 || d > o {
			return o, fmt.Errorf("%w: match distance %d at output offset %d", ErrCorrupt, d, o)
		}
		if len(dst)-o < m {
			return o, fmt.Errorf("%w: match at output offset %d", ErrDstTooSmall, o)
		}
		dPrev = d
		copyMatch(dst, o, d, m)
		o += m
	}
	return o, nil
}

// copyMatch copies m bytes from dst[o-d:] to dst[o:]. The regions may
// overlap (d < m), in which case the copy repeats the last d bytes, as LZ77
// requires. The caller guarantees 0 < d <= o and o+m <= len(dst).
func copyMatch(dst []byte, o, d, m int) {
	if d >= m {
		copy(dst[o:o+m], dst[o-d:])
		return
	}
	// Overlapping: double the copied span each round so that long runs with
	// a short period do not degrade to a byte-by-byte loop.
	end := o + m
	for o < end {
		n := copy(dst[o:end], dst[o-d:o])
		o += n
		d += n
	}
}

func truncated(at int) error {
	return fmt.Errorf("%w: truncated opcode at offset %d", ErrCorrupt, at)
}
