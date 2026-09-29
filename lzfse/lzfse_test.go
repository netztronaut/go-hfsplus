package lzfse

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestTables(t *testing.T) {
	check := func(name string, extra []uint8, base []int32, max int32) {
		t.Helper()
		for i := 1; i < len(base); i++ {
			if base[i] != base[i-1]+1<<extra[i-1] {
				t.Errorf("%s: base[%d] = %d, want %d", name, i, base[i], base[i-1]+1<<extra[i-1])
			}
		}
		last := len(base) - 1
		if got := base[last] + 1<<extra[last] - 1; got != max {
			t.Errorf("%s: largest value %d, want %d", name, got, max)
		}
	}
	check("L", lExtraBits[:], lBaseValue[:], 315)
	check("M", mExtraBits[:], mBaseValue[:], 2359)
	check("D", dExtraBits[:], dBaseValue[:], 262139)
}

// encodeFreq is the inverse of decodeFreq, written from the code table.
func encodeFreq(v int) (bits uint32, n int) {
	switch {
	case v <= 1:
		return uint32(v) << 1, 2
	case v <= 3:
		return 1 | uint32(v-2)<<2, 3
	case v <= 7:
		return 3 | uint32(v-4)<<3, 5
	case v <= 23:
		return 7 | uint32(v-8)<<4, 8
	default:
		return 15 | uint32(v-24)<<4, 14
	}
}

func TestDecodeFreq(t *testing.T) {
	for v := 0; v <= 24+1023; v++ {
		bits, n := encodeFreq(v)
		for _, junk := range []uint32{0, 0xffffffff, 0xa5a5a5a5} {
			got, gn := decodeFreq(bits | junk<<uint(n))
			if int(got) != v || gn != n {
				t.Fatalf("decodeFreq(encode(%d)) = %d, %d bits; want %d bits", v, got, gn, n)
			}
		}
	}
}

// TestSpreadStates checks, for assorted frequency tables, that every next
// state a decoder table can produce is below the state count, which is why
// the masking in decodeLit and decodeValue never changes a valid state.
func TestSpreadStates(t *testing.T) {
	p := prng(7)
	for _, nstates := range []int{64, 256, 1024} {
		for trial := 0; trial < 200; trial++ {
			nsym := 1 + p.intn(256)
			freq := make([]uint16, nsym)
			left := nstates
			if trial%2 == 0 {
				left -= p.intn(nstates) // leave some states unassigned
			}
			for left > 0 {
				k := 1 + p.intn(min(left, 1+nstates/8))
				freq[p.intn(nsym)] += uint16(k)
				left -= k
			}
			seen := 0
			spread(nstates, freq, func(state, sym int, k uint, delta int) {
				seen++
				if state >= nstates || freq[sym] == 0 {
					t.Fatalf("bad state %d for symbol %d", state, sym)
				}
				if delta < 0 || delta+(1<<k)-1 >= nstates {
					t.Fatalf("nstates %d: state %d reaches %d..%d", nstates, state, delta, delta+(1<<k)-1)
				}
			})
			sum := 0
			for _, f := range freq {
				sum += int(f)
			}
			if seen != sum {
				t.Fatalf("spread visited %d states, want %d", seen, sum)
			}
		}
	}
}

func TestBitReader(t *testing.T) {
	// A stream of 12 bits 1010_1100_1111 written LSB-first into bytes, as
	// the encoder does, with extra = 12%8 - 8 = -4 meaningful-bits
	// adjustment; preceded by 8 bytes of unrelated data the reader may load.
	buf := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xcf, 0x0a}
	var r bitReader
	if !r.init(buf, -4) {
		t.Fatal("init failed")
	}
	if got := r.pull(4); got != 0xa {
		t.Errorf("first 4 bits = %#x, want 0xa", got)
	}
	if got := r.pull(8); got != 0xcf {
		t.Errorf("next 8 bits = %#x, want 0xcf", got)
	}
	// Nonzero bits above the declared count are rejected.
	if r.init([]byte{0, 0, 0, 0, 0, 0, 0, 0x1a}, -4) {
		t.Error("init accepted garbage above the stream's last bit")
	}
	for _, extra := range []int32{1, -9, 100, -1 << 31} {
		if r.init(make([]byte, 16), extra) {
			t.Errorf("init accepted extra = %d", extra)
		}
	}
	if r.init(make([]byte, 7), -1) || r.init(make([]byte, 6), 0) {
		t.Error("init accepted a too short buffer")
	}
	if !r.init(make([]byte, 7), 0) {
		t.Error("init rejected a 7-byte buffer with extra = 0")
	}
	r.pull(20)
	if r.refill() {
		t.Error("refill read before the start of the buffer")
	}
}

func v2Header(nRaw uint32, v0, v1, v2 uint64) []byte {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint32(b, magicV2)
	binary.LittleEndian.PutUint32(b[4:], nRaw)
	binary.LittleEndian.PutUint64(b[8:], v0)
	binary.LittleEndian.PutUint64(b[16:], v1)
	binary.LittleEndian.PutUint64(b[24:], v2)
	return b
}

func TestMalformed(t *testing.T) {
	eos := []byte("bvx$")
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	stored := func(s string) []byte {
		b := make([]byte, 8, 8+len(s))
		binary.LittleEndian.PutUint32(b, magicUncompressed)
		binary.LittleEndian.PutUint32(b[4:], uint32(len(s)))
		return append(b, s...)
	}
	lzvnBlock := func(nRaw uint32, payload []byte) []byte {
		b := make([]byte, 12, 12+len(payload))
		binary.LittleEndian.PutUint32(b, magicLZVN)
		binary.LittleEndian.PutUint32(b[4:], nRaw)
		binary.LittleEndian.PutUint32(b[8:], uint32(len(payload)))
		return append(b, payload...)
	}
	lzvnABC := []byte{0xe3, 'a', 'b', 'c', 0x06, 0, 0, 0, 0, 0, 0, 0}

	tests := []struct {
		name string
		src  []byte
		dst  int
		want string // decoded output, or "" with err set
		err  error
	}{
		{"empty input", nil, 10, "", ErrCorrupt},
		{"eos only", eos, 10, "", nil},
		{"trailing data ignored", cat(stored("hi"), eos, []byte("garbage")), 10, "hi", nil},
		{"two stored blocks", cat(stored("ab"), stored("cd"), eos), 4, "abcd", nil},
		{"short magic", []byte("bvx"), 10, "", ErrCorrupt},
		{"unknown magic", cat([]byte("bvxz"), eos), 10, "", ErrCorrupt},
		{"stored truncated", stored("hello")[:10], 10, "", ErrCorrupt},
		{"stored header truncated", stored("hello")[:6], 10, "", ErrCorrupt},
		{"stored too large for dst", cat(stored("hello"), eos), 4, "", ErrDstTooSmall},
		{"missing eos", stored("hello"), 10, "", ErrCorrupt},
		{"lzvn ok", cat(lzvnBlock(3, lzvnABC), eos), 3, "abc", nil},
		{"lzvn raw size too big", cat(lzvnBlock(4, lzvnABC), eos), 4, "", ErrCorrupt},
		{"lzvn raw size too small", cat(lzvnBlock(2, lzvnABC), eos), 4, "", ErrCorrupt},
		{"lzvn raw size beyond dst", cat(lzvnBlock(3, lzvnABC), eos), 2, "", ErrDstTooSmall},
		{"lzvn payload truncated", lzvnBlock(3, lzvnABC)[:14], 3, "", ErrCorrupt},
		{"v2 fixed header truncated", v2Header(0, 0, 0, 32)[:31], 10, "", ErrCorrupt},
		{"v2 header size below 32", cat(v2Header(0, 0, 0, 31), eos), 10, "", ErrCorrupt},
		{"v2 header size beyond input", cat(v2Header(0, 0, 0, 1000), eos), 10, "", ErrCorrupt},
		{"v2 too many literals", cat(v2Header(1<<20, 40001, 0, 32), eos), 10, "", ErrCorrupt},
		{"v2 too many matches", cat(v2Header(1<<20, 10001<<40, 0, 32), eos), 10, "", ErrCorrupt},
		{"v2 more literals than output", cat(v2Header(1, 5, 0, 32), eos), 10, "", ErrCorrupt},
		{"v2 more matches than output", cat(v2Header(1, 2<<40, 0, 32), eos), 10, "", ErrCorrupt},
		{"v2 bad l state", cat(v2Header(0, 0, 0, 32|64<<32), eos), 10, "", ErrCorrupt},
		{"v2 bad m state", cat(v2Header(0, 0, 0, 32|64<<42), eos), 10, "", ErrCorrupt},
		{"v2 bad d state", cat(v2Header(0, 0, 0, 32|256<<52), eos), 10, "", ErrCorrupt},
		{"v2 payload beyond input", cat(v2Header(0, 100<<20, 0, 32), eos), 10, "", ErrCorrupt},
		{"v1 header truncated", cat([]byte("bvx1"), make([]byte, 700)), 10, "", ErrCorrupt},
		{"v2 raw size beyond dst", cat(v2Header(11, 0, 0, 32), eos), 10, "", ErrDstTooSmall},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := make([]byte, tt.dst)
			n, err := Decode(dst, tt.src)
			if n < 0 || n > len(dst) {
				t.Fatalf("n = %d out of range", n)
			}
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v, want %v", err, tt.err)
				}
				return
			}
			if err != nil || string(dst[:n]) != tt.want {
				t.Fatalf("Decode = %q, %v; want %q", dst[:n], err, tt.want)
			}
		})
	}
}

// TestMalformedFreqTables corrupts the frequency tables of a real v2 header.
func TestMalformedFreqTables(t *testing.T) {
	comp := readVector(t, "text-70k")
	var h header
	if err := h.parseV2(comp); err != nil {
		t.Fatal(err)
	}
	dst := make([]byte, 70000)

	// Header size one byte short or long: the tables no longer end exactly
	// at the header's end.
	for _, delta := range []int{-1, 1} {
		b := bytes.Clone(comp)
		v2 := binary.LittleEndian.Uint64(b[24:])
		binary.LittleEndian.PutUint64(b[24:], v2&^0xffffffff|uint64(int(v2&0xffffffff)+delta))
		if _, err := Decode(dst, b); !errors.Is(err, ErrCorrupt) {
			t.Errorf("header size %+d: err = %v, want ErrCorrupt", delta, err)
		}
	}

	// Frequencies that sum to more than the state count: rewrite the block
	// as v1 and inflate one literal frequency.
	v1 := v2ToV1(t, comp)
	off := 50 + 2*(litFreqOff+'e')
	binary.LittleEndian.PutUint16(v1[off:], binary.LittleEndian.Uint16(v1[off:])+1024)
	if _, err := Decode(dst, v1); !errors.Is(err, ErrCorrupt) {
		t.Errorf("oversubscribed literal table: err = %v, want ErrCorrupt", err)
	}

	// Omitted tables (header size 32) with nonzero counts decode garbage
	// or fail, but must not panic.
	b := bytes.Clone(comp)
	v2 := binary.LittleEndian.Uint64(b[24:])
	binary.LittleEndian.PutUint64(b[24:], v2&^0xffffffff|32)
	Decode(dst, b)
}
