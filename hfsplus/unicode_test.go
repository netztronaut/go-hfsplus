package hfsplus

import (
	"bufio"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// measuredDir holds what internal/gen/unicode measured on the macOS kernel.
const measuredDir = "../internal/gen/unicode/testdata"

func u16(s string) []uint16 { return utf16.Encode([]rune(s)) }

func TestNameToHFS(t *testing.T) {
	tests := []struct {
		in   string
		want []uint16
	}{
		{"abc", u16("abc")},
		{"\u00e9", []uint16{'e', 0x0301}},                            // precomposed e-acute is stored decomposed
		{"e\u0301", []uint16{'e', 0x0301}},                           // already decomposed
		{"a\u0301\u0316", []uint16{'a', 0x0316, 0x0301}},             // canonical order: 220 before 230
		{"\u00e9\u0316", []uint16{'e', 0x0316, 0x0301}},              // reordering across a decomposition
		{"\u1e69", []uint16{'s', 0x0323, 0x0307}},                    // recursive decomposition
		{"\uac00", []uint16{0x1100, 0x1161}},                         // Hangul LV
		{"\uac01", []uint16{0x1100, 0x1161, 0x11a8}},                 // Hangul LVT
		{"\u2126", []uint16{0x2126}},                                 // U+2000-2FFF excluded
		{"\u212b", []uint16{0x212b}},                                 // (even the Angstrom sign)
		{"\uf900", []uint16{0xf900}},                                 // U+F900-FAFF excluded
		{"\U0001d15e", []uint16{0xd834, 0xdd5e}},                     // supplementary: never decomposed
		{"x\u200cy", []uint16{'x', 0x200c, 'y'}},                     // ignorable, but kept
		{"x:y", []uint16{'x', '/', 'y'}},                             // POSIX ':' is stored '/'
		{"n\u2400n", []uint16{'n', 0, 'n'}},                          // U+2400 is stored NUL
		{"q\ufffe", u16("q%EF%BF%BE")},                               // noncharacters are escaped
		{"\U0001fffe", []uint16{0xd83f, 0xdffe}},                     // but not outside the BMP
		{"a\u0301\U0001d167", []uint16{'a', 0x0301, 0xd834, 0xdd67}}, // no reordering of supplementary marks
		{strings.Repeat("L", 255), u16(strings.Repeat("L", 255))},
		{strings.Repeat("\u00e9", 127), u16(strings.Repeat("e\u0301", 127))},
	}
	for _, tt := range tests {
		got, err := nameToHFS(tt.in)
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("nameToHFS(%+q) = %04X, %v; want %04X", tt.in, got, err, tt.want)
		}
	}
	bad := []string{
		"", "a/b", "/", "a\x00b",
		"bad\xff", "ov\xc0\xaf", "sur\xed\xa0\x80", "tr\xe9x", "big\xf4\x90\x80\x80",
		strings.Repeat("M", 256),
		strings.Repeat("\u00ea", 128),       // 256 units decomposed
		strings.Repeat("O", 255) + "\u200c", // ignorables count
		strings.Repeat("\U00010401", 128),   // surrogate pairs count twice
		strings.Repeat("\uac02", 86),        // 3 units per syllable
		strings.Repeat("\u00e9", 100000),    // long inputs fail without huge allocations
	}
	for _, in := range bad {
		if got, err := nameToHFS(in); !errors.Is(err, errInvalidName) {
			t.Errorf("nameToHFS(%+q) = %04X, %v; want errInvalidName", trunc(in), got, err)
		}
	}
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

func TestNameFromHFS(t *testing.T) {
	tests := []struct {
		in   []uint16
		want string
	}{
		{u16("abc"), "abc"},
		{u16("a/b"), "a:b"},
		{u16("a:b"), "a:b"}, // a stored ':' also reads as ':' (the kernel does the same)
		{[]uint16{'e', 0x0301}, "e\u0301"},
		{[]uint16{0x00e9}, "\u00e9"}, // no normalisation either way
		{[]uint16{0, 0, 0, 0, 'H', 'F', 'S', '+'}, "\u2400\u2400\u2400\u2400HFS+"},
		{[]uint16{0xd834, 0xdd5e}, "\U0001d15e"},
		{[]uint16{0xd800}, "\ufffd"},
		{[]uint16{'a', 0xdc00, 'b'}, "a\ufffdb"},
		{[]uint16{0xdc00, 0xd800}, "\ufffd\ufffd"},
		{[]uint16{0x2400}, "\u2400"},
		{nil, ""},
	}
	for _, tt := range tests {
		if got := nameFromHFS(tt.in); got != tt.want {
			t.Errorf("nameFromHFS(%04X) = %+q, want %+q", tt.in, got, tt.want)
		}
	}
}

func sign(x int) int {
	switch {
	case x < 0:
		return -1
	case x > 0:
		return 1
	}
	return 0
}

func TestCompareFold(t *testing.T) {
	priv := []uint16{0, 0, 0, 0, 'H', 'F', 'S', '+', ' ', 'P', 'r', 'i', 'v', 'a', 't', 'e', ' ', 'D', 'a', 't', 'a'}
	tests := []struct {
		a, b []uint16
		want int
	}{
		{u16("abc"), u16("ABC"), 0},
		{u16("abc"), u16("abd"), -1},
		{u16("ab"), u16("abc"), -1},     // the longer is greater
		{u16("a\u200cb"), u16("ab"), 0}, // ignorable
		{u16("a\ufeff"), u16("A"), 0},
		{u16("\u0391"), u16("\u03b1"), 0},          // Greek
		{u16("\u2126"), u16("\u03c9"), 1},          // OHM SIGN is not omega
		{u16("\u212a"), u16("k"), 1},               // nor KELVIN SIGN k
		{u16("\u0130"), u16("i"), 1},               // nor dotted capital I i
		{u16("\uff21"), u16("\uff41"), 0},          // fullwidth
		{u16("\u01c4"), u16("\u01c5"), 0},          // capital, title and small DZ with caron
		{u16("\ue000"), u16("z"), 1},               // unsigned
		{u16("\U00010400"), u16("\U00010428"), -1}, // no folding outside the BMP
		{priv, u16("\uffe0zzz"), 1},                // NUL folds to 0xFFFF: the private directory sorts last
		{[]uint16{0}, []uint16{0xffff}, 0},
		{u16("\u200c"), nil, 0},
		{nil, nil, 0},
	}
	for _, tt := range tests {
		if got := sign(compareFold(tt.a, tt.b)); got != tt.want {
			t.Errorf("compareFold(%04X, %04X) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
		if got := sign(compareFold(tt.b, tt.a)); got != -tt.want {
			t.Errorf("compareFold(%04X, %04X) = %d, want %d", tt.b, tt.a, got, -tt.want)
		}
	}
}

func TestCompareBinary(t *testing.T) {
	tests := []struct {
		a, b []uint16
		want int
	}{
		{u16("abc"), u16("abc"), 0},
		{u16("ABC"), u16("abc"), -1},
		{u16("ab"), u16("abc"), -1},
		{u16("a\u200cb"), u16("ab"), 1},
		{[]uint16{0xffff}, []uint16{0}, 1},
		{nil, nil, 0},
	}
	for _, tt := range tests {
		if got := sign(compareBinary(tt.a, tt.b)); got != tt.want {
			t.Errorf("compareBinary(%04X, %04X) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
		if got := sign(compareBinary(tt.b, tt.a)); got != -tt.want {
			t.Errorf("compareBinary(%04X, %04X) = %d, want %d", tt.b, tt.a, got, -tt.want)
		}
	}
}

func TestCompareNoAlloc(t *testing.T) {
	a, b := u16("Some Longer File Name\u200c.txt"), u16("some longer file name.TXT")
	if n := testing.AllocsPerRun(100, func() { compareFold(a, b); compareBinary(a, b) }); n != 0 {
		t.Errorf("compare allocates %v times", n)
	}
	if n := testing.AllocsPerRun(100, func() { nameFromHFS(a) }); n > 1 {
		t.Errorf("nameFromHFS allocates %v times", n)
	}
}

func TestTablesImmutableShape(t *testing.T) {
	if !slices.IsSorted(decompKeys[:]) || !slices.IsSorted(escapedRunes[:]) {
		t.Fatal("decompKeys or escapedRunes not sorted")
	}
	if int(decompStart[len(decompStart)-1]) != len(decompData) {
		t.Fatal("decompStart does not cover decompData")
	}
	for i, p := range foldIndex {
		if int(p)*256 > len(foldPages) {
			t.Fatalf("foldIndex[%d] out of range", i)
		}
	}
	for i, p := range cccIndex {
		if int(p)*256 > len(cccPages) {
			t.Fatalf("cccIndex[%d] out of range", i)
		}
	}
}

// --- tests against the kernel measurements ---

func readMeasured(t testing.TB, name string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(measuredDir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(name, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		r = zr
	}
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func hexList(t testing.TB, s string) []uint32 {
	t.Helper()
	if s == "-" {
		return nil
	}
	var out []uint32
	for _, f := range strings.Fields(s) {
		v, err := strconv.ParseUint(f, 16, 32)
		if err != nil {
			t.Fatalf("bad hex %q", s)
		}
		out = append(out, uint32(v))
	}
	return out
}

func hexUnits(t testing.TB, s string) []uint16 {
	var out []uint16
	for _, v := range hexList(t, s) {
		out = append(out, uint16(v))
	}
	return out
}

func hexRunes(t testing.TB, s string) string {
	var out []rune
	for _, v := range hexList(t, s) {
		out = append(out, rune(v))
	}
	return string(out)
}

// TestMeasuredDecomposition checks the stored form of every code point the
// kernel was given, one per file name.
func TestMeasuredDecomposition(t *testing.T) {
	listed := map[rune][]uint16{}
	for _, l := range readMeasured(t, "decomp.tsv.gz") {
		if l == "" || l[0] == '#' {
			continue
		}
		f := strings.Split(l, "\t")
		cp := hexList(t, f[0])[0]
		listed[rune(cp)] = hexUnits(t, f[1])
	}
	n := 0
	check := func(r rune, want []uint16) {
		n++
		got, err := nameToHFS(string(r))
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("nameToHFS(U+%04X) = %04X, %v; kernel stored %04X", r, got, err, want)
		}
	}
	for r := rune(1); r <= 0xFFFF; r++ {
		if r == '/' || r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		want, ok := listed[r]
		if !ok {
			want = []uint16{uint16(r)}
		}
		check(r, want)
	}
	for r, want := range listed {
		if r > 0xFFFF {
			check(r, want)
		}
	}
	if n < 64000 {
		t.Errorf("only %d code points checked", n)
	}
}

// TestMeasuredSequences checks composed inputs: decomposable characters
// followed by marks, targeted cases and random sequences.
func TestMeasuredSequences(t *testing.T) {
	n := 0
	for _, l := range readMeasured(t, "seq.tsv.gz") {
		f := strings.Split(l, "\t")
		in := hexRunes(t, f[0])
		got, err := nameToHFS(in)
		if strings.HasPrefix(f[1], "!") {
			if err == nil {
				t.Errorf("nameToHFS(%+q) succeeded; kernel: %s", in, f[1])
			}
			continue
		}
		n++
		if want := hexUnits(t, f[1]); err != nil || !slices.Equal(got, want) {
			t.Errorf("nameToHFS(%+q) = %04X, %v; kernel stored %04X", in, got, err, want)
		}
	}
	if n < 1000 {
		t.Errorf("only %d sequences", n)
	}
}

type measuredEntry struct {
	stored    []uint16
	input     string
	colliders []string
}

// readFold reads the compact fold/ign files (see measureFold in the generator).
func readFold(t *testing.T, name string) []measuredEntry {
	var pre, post string
	var out []measuredEntry
	for _, l := range readMeasured(t, name) {
		switch {
		case strings.HasPrefix(l, "#pre "):
			pre = hexRunes(t, l[5:])
		case strings.HasPrefix(l, "#post "):
			post = hexRunes(t, l[6:])
		case l == "" || l[0] == '#':
		case strings.HasPrefix(l, "R\t"):
			f := strings.Split(l, "\t")
			for v := hexList(t, f[1])[0]; v <= hexList(t, f[2])[0]; v++ {
				out = append(out, measuredEntry{stored: slices.Concat(u16(pre), []uint16{uint16(v)}, u16(post)), input: pre + string(rune(v)) + post})
			}
		default:
			f := strings.Split(l, "\t")
			e := measuredEntry{stored: slices.Concat(u16(pre), hexUnits(t, f[0]), u16(post)), input: pre + hexRunes(t, f[1]) + post}
			for _, c := range f[2:] {
				e.colliders = append(e.colliders, pre+hexRunes(t, c)+post)
			}
			out = append(out, e)
		}
	}
	return out
}

func readOrder(t *testing.T, name string) []measuredEntry {
	var out []measuredEntry
	for _, l := range readMeasured(t, name) {
		if l == "" || l[0] == '#' {
			continue
		}
		f := strings.Split(l, "\t")
		e := measuredEntry{stored: hexUnits(t, f[0]), input: hexRunes(t, f[1])}
		for _, c := range f[2:] {
			e.colliders = append(e.colliders, hexRunes(t, c))
		}
		out = append(out, e)
	}
	return out
}

// checkOrder checks that the entries, in the kernel's on-disk key order, are
// strictly increasing under cmp, that each was stored as nameToHFS predicts
// and that every input the kernel refused with EEXIST compares equal.
func checkOrder(t *testing.T, name string, es []measuredEntry, cmp func(a, b []uint16) int) {
	t.Helper()
	if len(es) < 1000 {
		t.Fatalf("%s: only %d entries", name, len(es))
	}
	fails := 0
	fail := func(format string, a ...any) {
		if fails++; fails < 20 {
			t.Errorf(name+": "+format, a...)
		}
	}
	for i, e := range es {
		if got, err := nameToHFS(e.input); err != nil || !slices.Equal(got, e.stored) {
			fail("nameToHFS(%+q) = %04X, %v; kernel stored %04X", e.input, got, err, e.stored)
		}
		if i > 0 && cmp(es[i-1].stored, e.stored) >= 0 {
			fail("kernel order %04X < %04X, compare says %d", es[i-1].stored, e.stored, cmp(es[i-1].stored, e.stored))
		}
		for _, c := range e.colliders {
			s, err := nameToHFS(c)
			if err != nil || cmp(s, e.stored) != 0 {
				fail("kernel: %+q collides with %04X; compare(%04X) = %d, %v", c, e.stored, s, cmp(s, e.stored), err)
			}
		}
	}
}

func TestMeasuredFoldOrder(t *testing.T) {
	checkOrder(t, "fold.tsv.gz", readFold(t, "fold.tsv.gz"), compareFold)
	checkOrder(t, "ign.tsv.gz", readFold(t, "ign.tsv.gz"), compareFold)
}

func TestMeasuredNameOrder(t *testing.T) {
	checkOrder(t, "order.tsv.gz", readOrder(t, "order.tsv.gz"), compareFold)
	checkOrder(t, "hfsx.tsv.gz", readOrder(t, "hfsx.tsv.gz"), compareBinary)
}

// TestMeasuredLookup checks units the kernel never stores itself, compared
// by lookups in catalogs patched to hold "a"+u+"b".
func TestMeasuredLookup(t *testing.T) {
	n := 0
	for _, l := range readMeasured(t, "lookup.tsv.gz") {
		f := strings.Split(l, "\t")
		u := uint16(hexList(t, f[0])[0])
		name := []uint16{'a', u, 'b'}
		for _, p := range f[1:] {
			k, v, _ := strings.Cut(p, "=")
			probe := []uint16{'a', 'b'}
			if k != "ab" {
				probe = []uint16{'a', uint16(hexList(t, k)[0]), 'b'}
			}
			n++
			if (compareFold(name, probe) == 0) != (v == "1") {
				t.Errorf("stored %04X, lookup %04X found=%s, compareFold = %d", name, probe, v, compareFold(name, probe))
			}
		}
	}
	if n < 10000 {
		t.Errorf("only %d lookups", n)
	}
}

// TestMeasuredProbes checks the kernel's comparison of stored names with
// names holding units it never writes, measured by B-tree leaf probes: each
// entry is the sign of compare(prefix+k, prefix+u).
func TestMeasuredProbes(t *testing.T) {
	n := 0
	for _, l := range readMeasured(t, "probe.tsv.gz") {
		if l == "" || l[0] == '#' {
			continue
		}
		f := strings.Split(l, "\t")
		u, p := uint16(hexList(t, f[0])[0]), uint16(hexList(t, f[1])[0])
		for _, r := range strings.Fields(f[2]) {
			k := uint16(hexList(t, r[:4])[0])
			want := map[byte]int{'<': -1, '=': 0, '>': 1}[r[4]]
			n++
			if got := sign(compareFold([]uint16{p, k}, []uint16{p, u})); got != want {
				t.Errorf("compareFold(%04X %04X, %04X %04X) = %d, kernel %c", p, k, p, u, got, r[4])
			}
		}
	}
	if n < 5000 {
		t.Errorf("only %d probes", n)
	}
}

// TestMeasuredPresent compares nameFromHFS with readdir of patched names.
func TestMeasuredPresent(t *testing.T) {
	for _, l := range readMeasured(t, "present.tsv") {
		f := strings.Split(l, "\t")
		stored := hexUnits(t, f[0])
		var kernel []byte
		for i := 0; i+1 < len(f[1]); i += 2 {
			v, _ := strconv.ParseUint(f[1][i:i+2], 16, 8)
			kernel = append(kernel, byte(v))
		}
		got := nameFromHFS(stored)
		switch {
		case slices.ContainsFunc(stored, func(c uint16) bool { return c >= 0xD800 && c < 0xE000 }) && !utf8.Valid(kernel):
			// The kernel writes unpaired surrogates as their 3-byte (invalid UTF-8)
			// encoding; nameFromHFS uses U+FFFD so names stay valid UTF-8.
			if !strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("nameFromHFS(%04X) = %+q, want U+FFFD for the unpaired surrogate", stored, got)
			}
		case slices.Contains(stored, 0xFFFE) || slices.Contains(stored, 0xFFFF):
			// The kernel drops stored noncharacters from what readdir returns
			// (the name can then be empty); nameFromHFS keeps them.
		case got != string(kernel):
			t.Errorf("nameFromHFS(%04X) = %+q, kernel readdir %+q", stored, got, kernel)
		}
	}
}

// --- fuzzing ---

// refDecomposed is an independent spelling of the POSIX form nameToHFS then
// nameFromHFS produce: per rune, the stored form from the tables, then a
// naive canonical ordering, then back to runes.
func refDecomposed(s string) string {
	var units []uint16
	for _, r := range s {
		switch {
		case r == ':':
			units = append(units, '/')
		case r == 0x2400:
			units = append(units, 0)
		case slices.Contains(escapedRunes[:], r):
			for _, b := range []byte(string(r)) {
				units = append(units, u16("%"+strings.ToUpper(strconv.FormatUint(uint64(b)|0x100, 16)[1:]))...)
			}
		case r >= 0xAC00 && r <= 0xD7A3:
			x := r - 0xAC00
			units = append(units, uint16(0x1100+x/588), uint16(0x1161+x%588/28))
			if x%28 != 0 {
				units = append(units, uint16(0x11A7+x%28))
			}
		case r > 0xFFFF:
			units = append(units, utf16.Encode([]rune{r})...)
		default:
			i, ok := slices.BinarySearch(decompKeys[:], uint16(r))
			if ok {
				units = append(units, decompData[decompStart[i]:decompStart[i+1]]...)
			} else {
				units = append(units, uint16(r))
			}
		}
	}
	for swapped := true; swapped; { // bubble sort adjacent marks
		swapped = false
		for i := 1; i < len(units); i++ {
			a, b := combiningClass(units[i-1]), combiningClass(units[i])
			if b != 0 && a > b {
				units[i-1], units[i] = units[i], units[i-1]
				swapped = true
			}
		}
	}
	for i, c := range units {
		switch c {
		case '/':
			units[i] = ':'
		case 0:
			units[i] = 0x2400
		}
	}
	return string(utf16.Decode(units))
}

func FuzzNameToHFS(f *testing.F) {
	for _, s := range []string{"a", "\u00e9", "a\u0301\u0316", "x:y", "\uac00\u11a8", "\U0001d15e", "q\ufffe", "n\u2400", "bad\xff", "a/b", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, err := nameToHFS(s)
		valid := s != "" && utf8.ValidString(s) && !strings.ContainsAny(s, "/\x00")
		if err != nil {
			if !errors.Is(err, errInvalidName) {
				t.Fatalf("nameToHFS(%+q): error %v does not wrap errInvalidName", s, err)
			}
			if valid && len(utf16.Encode([]rune(refDecomposed(s)))) <= 255 {
				t.Fatalf("nameToHFS(%+q): unexpected error %v", s, err)
			}
			return
		}
		if !valid {
			t.Fatalf("nameToHFS(%+q) accepted an invalid name", s)
		}
		if len(u) == 0 || len(u) > 255 {
			t.Fatalf("nameToHFS(%+q): %d units", s, len(u))
		}
		if slices.Contains(u, ':') {
			t.Fatalf("nameToHFS(%+q) = %04X contains ':'", s, u)
		}
		back := nameFromHFS(u)
		if want := refDecomposed(s); back != want {
			t.Fatalf("nameFromHFS(nameToHFS(%+q)) = %+q, want %+q", s, back, want)
		}
		again, err := nameToHFS(back)
		if err != nil || !slices.Equal(again, u) {
			t.Fatalf("nameToHFS not idempotent on %+q: %04X then %04X, %v", s, u, again, err)
		}
	})
}

func unitsOf(b []byte) []uint16 {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return u
}

// refCompareFold folds both names completely, drops ignorables and compares
// the results lexicographically.
func refCompareFold(a, b []uint16) int {
	f := func(u []uint16) []uint16 {
		var out []uint16
		for _, c := range u {
			if v := foldUnit(c); v != 0 {
				out = append(out, v)
			}
		}
		return out
	}
	return slices.Compare(f(a), f(b))
}

func FuzzCompareFold(f *testing.F) {
	f.Add([]byte("\x00a\x00b"), []byte("\x00A\x00B"))
	f.Add([]byte("\x20\x0c\x00a"), []byte("\x00a"))
	f.Add([]byte("\x00\x00"), []byte("\xff\xff"))
	f.Fuzz(func(t *testing.T, x, y []byte) {
		a, b := unitsOf(x), unitsOf(y)
		ab, ba := compareFold(a, b), compareFold(b, a)
		if sign(ab) != -sign(ba) {
			t.Fatalf("compareFold(%04X, %04X) = %d but reversed %d", a, b, ab, ba)
		}
		if compareFold(a, a) != 0 || compareFold(b, b) != 0 {
			t.Fatalf("compareFold not reflexive on %04X / %04X", a, b)
		}
		if r := refCompareFold(a, b); sign(ab) != r {
			t.Fatalf("compareFold(%04X, %04X) = %d, reference %d", a, b, ab, r)
		}
		if sign(compareBinary(a, b)) != slices.Compare(a, b) {
			t.Fatalf("compareBinary(%04X, %04X) = %d", a, b, compareBinary(a, b))
		}
	})
}

// --- benchmarks ---

func BenchmarkCompareFold(b *testing.B) {
	x, y := u16("Application Support"), u16("application support")
	for b.Loop() {
		compareFold(x, y)
	}
}

func BenchmarkNameToHFS(b *testing.B) {
	for b.Loop() {
		nameToHFS("Caf\u00e9 cr\u00e8me.txt")
	}
}

func BenchmarkNameFromHFS(b *testing.B) {
	u := u16("Library")
	for b.Loop() {
		nameFromHFS(u)
	}
}
