package lzvn

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func cat(parts ...any) []byte {
	var b []byte
	for _, p := range parts {
		switch p := p.(type) {
		case int:
			b = append(b, byte(p))
		case string:
			b = append(b, p...)
		case []byte:
			b = append(b, p...)
		default:
			panic(fmt.Sprintf("cat: %T", p))
		}
	}
	return b
}

var eos = []byte{0x06, 0, 0, 0, 0, 0, 0, 0}

func TestOpcodes(t *testing.T) {
	lit271 := bytes.Repeat([]byte("0123456789"), 28)[:271]
	lit9 := "ABCDEFGHI"
	hist280 := string(lit271) + lit9

	tests := []struct {
		name string
		src  []byte
		want string
	}{
		{"empty", nil, ""},
		{"eos only", eos, ""},
		{"sml_l", cat(0xe3, "abc", eos), "abc"},
		{"lrg_l", cat(0xe0, 4, "abcdefghijklmnopqrst", eos), "abcdefghijklmnopqrst"},
		// sml_d L=1 M=3 D=1: one literal, then three copies of it.
		{"sml_d", cat(0x40, 0x01, "x", eos), "xxxx"},
		// sml_d L=0 M=10 D=0x102 needs 258 bytes of history.
		{"sml_d high distance", cat(0xe0, 0xf2, string(lit271[:258]), 0x39, 0x02, eos),
			string(lit271[:258]) + string(lit271[:10])},
		// med_d L=2 M=20 D=3: 101 LL=10 MMM=100 | DDDDDD=000011 MM=01 | 0.
		{"med_d", cat(0xe1, "c", 0xb4, 0x0d, 0x00, "ab", eos), "cab" + strings.Repeat("cab", 7)[:20]},
		// lrg_d L=0 M=10 D=280.
		{"lrg_d", cat(0xe0, 0xff, lit271, 0xe9, lit9, 0x3f, 0x18, 0x01, eos), hist280 + hist280[:10]},
		// pre_d L=1 M=3 reuses D=1; sml_m M=2 and lrg_m M=16 as well.
		{"pre_d sml_m lrg_m", cat(0x40, 0x01, "x", 0x46, "y", 0xf2, 0xf0, 0x00, eos),
			"xxxx" + "yyyy" + "yy" + strings.Repeat("y", 16)},
		{"nops", cat(0x0e, 0xe1, "a", 0x16, 0x0e, 0xe1, "b", eos), "ab"},
		// sml_d L=2 M=6 D=2: an overlapping copy with period 2.
		{"overlap", cat(0x98, 0x02, "ab", eos), "abababab"},
		{"eos without padding", cat(0xe1, "a", 0x06), "a"},
		{"data after eos ignored", cat(0xe1, "a", eos, 0xe1, "b"), "a"},
		{"no eos", cat(0xe1, "a", 0xe1, "b"), "ab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := make([]byte, 1000)
			n, err := Decode(dst, tt.src)
			if err != nil || string(dst[:n]) != tt.want {
				t.Fatalf("Decode = %q, %v; want %q", dst[:n], err, tt.want)
			}
			// Exactly sized destination.
			dst = make([]byte, len(tt.want))
			if n, err := Decode(dst, tt.src); err != nil || string(dst[:n]) != tt.want {
				t.Fatalf("Decode into exact dst = %q, %v", dst[:n], err)
			}
		})
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name string
		src  []byte
		dst  int
		err  error
	}{
		{"pre_d without previous distance", cat(0x46, "a", eos), 10, ErrCorrupt},
		{"sml_m without previous distance", cat(0xe1, "a", 0xf1, eos), 10, ErrCorrupt},
		{"lrg_m without previous distance", cat(0xe1, "a", 0xf0, 0x01, eos), 10, ErrCorrupt},
		{"distance 0", cat(0xe1, "a", 0x00, 0x00, eos), 10, ErrCorrupt},
		{"distance beyond output", cat(0xe1, "a", 0x00, 0x02, eos), 10, ErrCorrupt},
		{"distance beyond output after literals", cat(0x40, 0x02, "a", eos), 10, ErrCorrupt},
		{"truncated sml_l", cat(0xe3, "ab"), 10, ErrCorrupt},
		{"truncated lrg_l opcode", cat(0xe0), 10, ErrCorrupt},
		{"truncated lrg_l literals", cat(0xe0, 0x00, "abc"), 100, ErrCorrupt},
		{"truncated sml_d", cat(0xe1, "a", 0x00), 10, ErrCorrupt},
		{"truncated sml_d literals", cat(0xc0, 0x01, "ab"), 10, ErrCorrupt},
		{"truncated med_d", cat(0xe1, "a", 0xa0, 0x04), 10, ErrCorrupt},
		{"truncated lrg_d", cat(0xe1, "a", 0x07, 0x01), 10, ErrCorrupt},
		{"truncated lrg_m", cat(0xe1, "a", 0x00, 0x01, 0xf0), 10, ErrCorrupt},
		{"literals beyond dst", cat(0xe3, "abc", eos), 2, ErrDstTooSmall},
		{"match beyond dst", cat(0x40, 0x01, "x", eos), 3, ErrDstTooSmall},
		{"more output after dst is full", cat(0xe3, "abc", 0xe1, "d", eos), 3, ErrDstTooSmall},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := make([]byte, tt.dst)
			n, err := Decode(dst, tt.src)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if n < 0 || n > len(dst) {
				t.Fatalf("n = %d out of range", n)
			}
		})
	}

	// A full destination followed by nops and the end of the stream is
	// still a success.
	dst := make([]byte, 3)
	if n, err := Decode(dst, cat(0xe3, "abc", 0x0e, 0x16)); err != nil || n != 3 {
		t.Errorf("full dst then nops = %d, %v", n, err)
	}
}

// TestUndefinedOpcodes checks the opcode map: exactly these 37 opcodes are
// undefined.
func TestUndefinedOpcodes(t *testing.T) {
	undefined := map[int]bool{0x1e: true, 0x26: true, 0x2e: true, 0x36: true, 0x3e: true}
	for op := 0x70; op <= 0x7f; op++ {
		undefined[op] = true
	}
	for op := 0xd0; op <= 0xdf; op++ {
		undefined[op] = true
	}
	tail := bytes.Repeat([]byte{0xe0}, 300) // lrg_l runs, so later bytes never matter
	for op := 0; op < 256; op++ {
		_, err := Decode(make([]byte, 4096), cat(op, 0x01, 0x01, tail))
		msg := fmt.Sprintf("undefined opcode 0x%02x at offset 0", op)
		got := err != nil && strings.HasSuffix(err.Error(), msg)
		if got != undefined[op] {
			t.Errorf("opcode 0x%02x: err = %v, undefined = %v", op, err, undefined[op])
		}
		if got && !errors.Is(err, ErrCorrupt) {
			t.Errorf("opcode 0x%02x: error does not wrap ErrCorrupt", op)
		}
	}
}

func TestCopyMatch(t *testing.T) {
	for d := 1; d <= 9; d++ {
		for m := 0; m <= 40; m++ {
			buf := make([]byte, 10+m)
			for i := range 10 {
				buf[i] = byte('a' + i)
			}
			want := bytes.Clone(buf)
			for i := 10; i < 10+m; i++ { // the obvious byte-by-byte copy
				want[i] = want[i-d]
			}
			copyMatch(buf, 10, d, m)
			if !bytes.Equal(buf, want) {
				t.Fatalf("d=%d m=%d: %q, want %q", d, m, buf, want)
			}
		}
	}
}
