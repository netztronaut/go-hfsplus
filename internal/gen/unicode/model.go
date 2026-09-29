package main

import (
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
)

// tables is the in-memory form of the generated tables together with the
// same algorithms hfsplus/unicode.go implements, so the generator can verify
// what it emits against the measurements before writing it.
type tables struct {
	decomp  map[uint16][]uint16 // BMP code point -> stored form (not Hangul syllables, not specials)
	escaped []rune              // stored as "%XX" escapes of their UTF-8 bytes
	ccc     [0x10000]uint8
	fold    [0x10000]uint16
}

const (
	sBase, lBase, vBase, tBase = 0xAC00, 0x1100, 0x1161, 0x11A7
	tCount                     = 28
	nCount                     = 21 * tCount
	sCount                     = 19 * nCount
)

var errModel = errors.New("invalid name")

func hangul(r rune) []uint16 {
	x := r - sBase
	out := []uint16{uint16(lBase + x/nCount), uint16(vBase + x%nCount/tCount)}
	if t := x % tCount; t != 0 {
		out = append(out, uint16(tBase+t))
	}
	return out
}

func escapeRune(r rune) []uint16 {
	var out []uint16
	for _, b := range []byte(string(r)) {
		out = append(out, '%', uint16("0123456789ABCDEF"[b>>4]), uint16("0123456789ABCDEF"[b&15]))
	}
	return out
}

// toHFS mirrors nameToHFS.
func (t *tables) toHFS(s string) ([]uint16, error) {
	if s == "" || !utf8.ValidString(s) || strings.ContainsAny(s, "/\x00") {
		return nil, errModel
	}
	var out []uint16
	for _, r := range s {
		switch {
		case r == ':':
			out = append(out, '/')
		case r == 0x2400:
			out = append(out, 0)
		case r >= sBase && r < sBase+sCount:
			out = append(out, hangul(r)...)
		case slices.Contains(t.escaped, r):
			out = append(out, escapeRune(r)...)
		case r > 0xFFFF:
			r -= 0x10000
			out = append(out, uint16(0xD800+r>>10), uint16(0xDC00+r&0x3FF))
		default:
			if d, ok := t.decomp[uint16(r)]; ok {
				out = append(out, d...)
			} else {
				out = append(out, uint16(r))
			}
		}
	}
	if len(out) > 255 {
		return nil, errModel
	}
	t.order(out)
	return out, nil
}

func (t *tables) order(u []uint16) {
	for i := 1; i < len(u); i++ {
		c := t.ccc[u[i]]
		if c == 0 {
			continue
		}
		for j := i; j > 0 && t.ccc[u[j-1]] > c; j-- {
			u[j], u[j-1] = u[j-1], u[j]
		}
	}
}

// compare is FastUnicodeCompare with t.fold.
func (t *tables) compare(a, b []uint16) int {
	i, j := 0, 0
	for {
		var c1, c2 uint16
		for c1 == 0 && i < len(a) {
			c1 = t.fold[a[i]]
			i++
		}
		for c2 == 0 && j < len(b) {
			c2 = t.fold[b[j]]
			j++
		}
		if c1 != c2 {
			if c1 < c2 {
				return -1
			}
			return 1
		}
		if c1 == 0 {
			return 0
		}
	}
}

func compareBinary(a, b []uint16) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// fromHFS mirrors nameFromHFS.
func fromHFS(u []uint16) string {
	var b strings.Builder
	for i := 0; i < len(u); i++ {
		c := u[i]
		switch {
		case c == '/':
			b.WriteByte(':')
		case c == 0:
			b.WriteRune(0x2400)
		case c >= 0xD800 && c < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000:
			b.WriteRune(0x10000 + (rune(c)-0xD800)<<10 + rune(u[i+1]) - 0xDC00)
			i++
		case c >= 0xD800 && c < 0xE000:
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteRune(rune(c))
		}
	}
	return b.String()
}

func utf16Of(s string) []uint16 {
	var out []uint16
	for _, r := range s {
		if r > 0xFFFF {
			r -= 0x10000
			out = append(out, uint16(0xD800+r>>10), uint16(0xDC00+r&0x3FF))
		} else {
			out = append(out, uint16(r))
		}
	}
	return out
}
