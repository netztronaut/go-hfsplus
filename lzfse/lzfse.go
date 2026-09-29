// Package lzfse decodes LZFSE streams.
//
// An LZFSE stream is a sequence of blocks, each introduced by a four-byte
// magic: "bvx-" (stored), "bvx1" and "bvx2" (LZ77 with finite-state-entropy
// coding, with an uncompressed or a packed header), "bvxn" (LZVN) and the
// end-of-stream marker "bvx$". The format implemented here follows Apple's
// reference implementation (lzfse_decode_base.c, lzfse_fse.c and
// lzfse_internal.h in the BSD-licensed lzfse project); the code is an
// independent implementation.
//
// The decoder is written for untrusted input: every read of src and every
// match copy is bounds-checked, it never writes beyond len(dst), and apart
// from a fixed scratch area of about 50 KiB (allocated only if the stream
// contains FSE-compressed blocks) it allocates nothing. Every block consumes
// at least eight bytes of src, so decoding terminates.
package lzfse

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/netztronaut/go-hfsplus-reader/lzvn"
)

// ErrCorrupt is returned (possibly wrapped) for malformed input.
var ErrCorrupt = errors.New("lzfse: corrupt input")

// ErrDstTooSmall is returned when the decoded stream would exceed len(dst).
var ErrDstTooSmall = errors.New("lzfse: output larger than destination")

// Block magics, little-endian.
const (
	magicEOS          = 0x24787662 // "bvx$"
	magicUncompressed = 0x2d787662 // "bvx-"
	magicV1           = 0x31787662 // "bvx1"
	magicV2           = 0x32787662 // "bvx2"
	magicLZVN         = 0x6e787662 // "bvxn"
)

// Decode decodes an LZFSE stream (a sequence of blocks: "bvx-" uncompressed,
// "bvx1" v1 compressed, "bvx2" v2 compressed, "bvxn" lzvn, terminated by
// "bvx$") from src into dst, returning bytes written.
//
// Decoding stops successfully at the "bvx$" block; bytes after it are
// ignored. A stream that ends without "bvx$", an unknown block magic, or any
// inconsistency inside a block is reported with an error wrapping
// ErrCorrupt. A block whose declared size does not fit in the rest of dst is
// reported with an error wrapping ErrDstTooSmall, before any of it is
// decoded. Every block must decode to exactly the number of bytes its
// header declares.
//
// Matches in FSE-compressed blocks may reach back into the output of
// earlier blocks, as the format allows. An LZVN block is decoded with
// lzvn.Decode, whose history is the block itself; Apple's encoder emits LZVN
// blocks only as the sole block of small inputs, so this does not limit
// real streams.
//
// On error the returned count is the number of bytes already written to
// dst; their content must not be trusted.
func Decode(dst, src []byte) (int, error) {
	var (
		s   int      // read position in src
		o   int      // write position in dst
		dec *decoder // scratch for FSE blocks, allocated on first use
	)
	for {
		if len(src)-s < 4 {
			return o, fmt.Errorf("%w: stream ends at offset %d without end-of-stream block", ErrCorrupt, s)
		}
		magic := binary.LittleEndian.Uint32(src[s:])
		switch magic {
		case magicEOS:
			return o, nil

		case magicUncompressed:
			if len(src)-s < 8 {
				return o, truncatedHeader(s)
			}
			n := uint64(binary.LittleEndian.Uint32(src[s+4:]))
			if n > uint64(len(src)-s-8) {
				return o, fmt.Errorf("%w: stored block at offset %d truncated", ErrCorrupt, s)
			}
			if n > uint64(len(dst)-o) {
				return o, dstTooSmall(s)
			}
			o += copy(dst[o:], src[s+8:s+8+int(n)])
			s += 8 + int(n)

		case magicLZVN:
			if len(src)-s < 12 {
				return o, truncatedHeader(s)
			}
			nRaw := uint64(binary.LittleEndian.Uint32(src[s+4:]))
			nPayload := uint64(binary.LittleEndian.Uint32(src[s+8:]))
			if nPayload > uint64(len(src)-s-12) {
				return o, fmt.Errorf("%w: lzvn block at offset %d truncated", ErrCorrupt, s)
			}
			if nRaw > uint64(len(dst)-o) {
				return o, dstTooSmall(s)
			}
			payload := src[s+12 : s+12+int(nPayload)]
			n, err := lzvn.Decode(dst[o:o+int(nRaw)], payload)
			if err != nil {
				return o + n, fmt.Errorf("%w: lzvn block at offset %d: %w", ErrCorrupt, s, err)
			}
			if n != int(nRaw) {
				return o + n, fmt.Errorf("%w: lzvn block at offset %d decoded to %d bytes, header says %d", ErrCorrupt, s, n, nRaw)
			}
			o += n
			s += 12 + int(nPayload)

		case magicV1, magicV2:
			var h header
			var err error
			if magic == magicV1 {
				err = h.parseV1(src[s:])
			} else {
				err = h.parseV2(src[s:])
			}
			if err != nil {
				return o, fmt.Errorf("%w: block at offset %d: %w", ErrCorrupt, s, err)
			}
			if uint64(h.nRawBytes) > uint64(len(dst)-o) {
				return o, dstTooSmall(s)
			}
			if dec == nil {
				dec = new(decoder)
			}
			n, used, err := dec.decodeBlock(dst, o, src[s:], &h)
			if err != nil {
				return o + n, fmt.Errorf("%w: block at offset %d: %w", ErrCorrupt, s, err)
			}
			o += n
			s += used

		default:
			return o, fmt.Errorf("%w: unknown block magic 0x%08x at offset %d", ErrCorrupt, magic, s)
		}
	}
}

func truncatedHeader(at int) error {
	return fmt.Errorf("%w: block header at offset %d truncated", ErrCorrupt, at)
}

func dstTooSmall(at int) error {
	return fmt.Errorf("%w: block at offset %d", ErrDstTooSmall, at)
}
