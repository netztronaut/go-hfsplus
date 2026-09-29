//go:build darwin

package main

// Measurement of the running macOS kernel's HFS+ name handling. Everything
// here creates names through POSIX on hdiutil images and reads back what the
// kernel stored by parsing the raw catalog of the detached image.

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

const (
	probeHigh = 0x0345 // combining class 240 in Unicode 3.2, the highest
	probeLow  = 0x0334 // combining class 1 in Unicode 3.2, the lowest
)

func errnoName(e syscall.Errno) string {
	switch e {
	case syscall.EEXIST:
		return "EEXIST"
	case syscall.ENAMETOOLONG:
		return "ENAMETOOLONG"
	case syscall.EINVAL:
		return "EINVAL"
	case syscall.EILSEQ:
		return "EILSEQ"
	}
	return fmt.Sprintf("E%d", int(e))
}

// stripPrefix removes the ASCII prefix p from a stored name.
func stripPrefix(s []uint16, p string) ([]uint16, error) {
	if len(s) < len(p) {
		return nil, fmt.Errorf("stored %s lacks prefix %q", fmtUnits(s), p)
	}
	for i := 0; i < len(p); i++ {
		if s[i] != uint16(p[i]) {
			return nil, fmt.Errorf("stored %s lacks prefix %q", fmtUnits(s), p)
		}
	}
	return s[len(p):], nil
}

// prefixedBatch creates prefix(i)+body[i] for each body and returns the
// stored bodies (prefix removed), or an error marker string per entry.
func prefixedBatch(work, name string, sizeMB int, bodies []string) ([][]uint16, []string, error) {
	names := make([]string, len(bodies))
	pfx := make([]string, len(bodies))
	for i, b := range bodies {
		pfx[i] = fmt.Sprintf("%05X_", i)
		names[i] = pfx[i] + b
	}
	br, err := runBatch(work, name, "HFS+", sizeMB, names)
	if err != nil {
		return nil, nil, err
	}
	out := make([][]uint16, len(bodies))
	errs := make([]string, len(bodies))
	for i, r := range br.Results {
		if r.Errno != 0 {
			errs[i] = "!" + errnoName(r.Errno)
			continue
		}
		s, err := stripPrefix(br.Stored[i], pfx[i])
		if err != nil {
			return nil, nil, err
		}
		out[i] = s
	}
	return out, errs, nil
}

func suppTestSet(u *ucd) []rune {
	set := map[rune]bool{0x10000: true, 0x10FFFD: true, 0x1D15E: true}
	for p := rune(1); p <= 16; p++ {
		set[p<<16|0xFFFE], set[p<<16|0xFFFF] = true, true
	}
	for r, c := range u.chars {
		if r > 0xFFFF && (c.Decomp != nil || c.CCC != 0 || c.Lower != 0 || c.Upper != 0) {
			set[r] = true
		}
	}
	for r := rune(0x10000); r <= 0x10FFFF; r += 0x1003 {
		set[r] = true
	}
	var out []rune
	for r := range set {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

type measurement struct {
	work, testdata string
	u              *ucd
	decomp         map[rune][]uint16 // BMP (and tested supplementary) code point -> stored units
	storable       map[uint16]rune   // unit -> an input code point stored as exactly that unit
	nonzero        []uint16          // units the kernel reorders (non-zero combining class)
	foldX          []orderEntry      // the "x"+c directory in on-disk order
	facts          []string
}

func runMeasure(work, testdata string, u *ucd) error {
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	m := &measurement{work: work, testdata: testdata, u: u}
	steps := []struct {
		name string
		fn   func() error
	}{
		{"meta", m.meta},
		{"decomp", m.measureDecomp},
		{"ccc", m.measureCCC},
		{"seq", m.measureSeq},
		{"fold", m.measureFold},
		{"order", m.measureOrder},
		{"present", m.measurePresent},
		{"lookup", m.measureLookup},
		{"probe", m.measureProbe},
		{"facts", m.measureFacts},
	}
	for _, s := range steps {
		t := time.Now()
		fmt.Fprintf(os.Stderr, "measure %s...\n", s.name)
		if err := s.fn(); err != nil {
			return fmt.Errorf("measure %s: %w", s.name, err)
		}
		fmt.Fprintf(os.Stderr, "measure %s: %v\n", s.name, time.Since(t).Round(time.Second))
	}
	return nil
}

func (m *measurement) meta() error {
	pv, _ := exec.Command("sw_vers", "-productVersion").Output()
	bv, _ := exec.Command("sw_vers", "-buildVersion").Output()
	kr, _ := exec.Command("uname", "-r").Output()
	hfs, _ := exec.Command("sh", "-c", "kextstat -l -b com.apple.filesystems.hfs.kext 2>/dev/null | awk '{print $6, $7}'").Output()
	lines := []string{
		"date\t" + time.Now().UTC().Format("2006-01-02"),
		"macos\t" + strings.TrimSpace(string(pv)) + " (" + strings.TrimSpace(string(bv)) + ")",
		"darwin\t" + strings.TrimSpace(string(kr)),
		"hfs_kext\t" + strings.TrimSpace(string(hfs)),
	}
	return writeLines(filepath.Join(m.testdata, "meta.tsv"), lines)
}

// measureDecomp stores every BMP code point (except surrogates, U+0000 and
// '/') and a supplementary sample, one file each, and records the stored form.
func (m *measurement) measureDecomp() error {
	var cps []rune
	for r := rune(1); r <= 0xFFFF; r++ {
		if r == '/' || (r >= 0xD800 && r <= 0xDFFF) {
			continue
		}
		cps = append(cps, r)
	}
	cps = append(cps, suppTestSet(m.u)...)
	bodies := make([]string, len(cps))
	for i, r := range cps {
		bodies[i] = string(r)
	}
	st, errs, err := prefixedBatch(m.work, "decomp", 400, bodies)
	if err != nil {
		return err
	}
	m.decomp = map[rune][]uint16{}
	m.storable = map[uint16]rune{}
	var lines []string
	for i, r := range cps {
		if errs[i] != "" {
			lines = append(lines, fmt.Sprintf("%04X\t%s", r, errs[i]))
			continue
		}
		m.decomp[r] = st[i]
		if r > 0xFFFF || len(st[i]) != 1 || rune(st[i][0]) != r {
			lines = append(lines, fmt.Sprintf("%04X\t%s", r, fmtUnits(st[i])))
		}
		if len(st[i]) == 1 {
			if _, ok := m.storable[st[i][0]]; !ok || rune(st[i][0]) == r {
				m.storable[st[i][0]] = r
			}
		}
	}
	hdr := []string{"# Stored form of every code point U+0001..U+FFFF except U+D800..U+DFFF and '/',",
		"# and of the supplementary code points listed. BMP code points not listed are stored as themselves."}
	return writeLines(filepath.Join(m.testdata, "decomp.tsv.gz"), append(hdr, lines...))
}

// identityUnits are the BMP units stored as themselves.
func (m *measurement) identityUnits() []uint16 {
	var out []uint16
	for v, r := range m.storable {
		if rune(v) == r {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

func swapped(stored []uint16, first, second uint16) (bool, error) {
	// stored must be "a" followed by the two marks in some order.
	if len(stored) != 3 || stored[0] != 'a' {
		return false, fmt.Errorf("unexpected stored %s", fmtUnits(stored))
	}
	switch {
	case stored[1] == first && stored[2] == second:
		return false, nil
	case stored[1] == second && stored[2] == first:
		return true, nil
	}
	return false, fmt.Errorf("unexpected stored %s for %04X %04X", fmtUnits(stored), first, second)
}

// measureCCC finds the units the kernel treats as combining (it reorders
// them) and their relative class order.
func (m *measurement) measureCCC() error {
	ids := m.identityUnits()
	if m.storable[probeHigh] != probeHigh || m.storable[probeLow] != probeLow {
		return fmt.Errorf("probe marks are not stored as themselves")
	}
	var bodies []string
	for _, v := range ids {
		bodies = append(bodies, "a"+string(rune(probeHigh))+string(rune(v)), "a"+string(rune(v))+string(rune(probeLow)))
	}
	st, errs, err := prefixedBatch(m.work, "ccc1", 600, bodies)
	if err != nil {
		return err
	}
	lines := []string{fmt.Sprintf("high\t%04X", probeHigh), fmt.Sprintf("low\t%04X", probeLow)}
	for i, v := range ids {
		if errs[2*i] != "" || errs[2*i+1] != "" {
			return fmt.Errorf("ccc probe of %04X failed: %s %s", v, errs[2*i], errs[2*i+1])
		}
		if v == probeHigh || v == probeLow {
			// a probe against itself never reorders; classify it by the other probe
		}
		s1, err := swapped(st[2*i], probeHigh, v)
		if err != nil {
			return err
		}
		s2, err := swapped(st[2*i+1], v, probeLow)
		if err != nil {
			return err
		}
		if s1 || s2 {
			m.nonzero = append(m.nonzero, v)
			lines = append(lines, fmt.Sprintf("nz\t%04X\t%d%d", v, b2i(s1), b2i(s2)))
		}
	}
	// The probes themselves: 0345 is found by P2 (240 > 1), 0334 by P1.
	nz := map[uint16]bool{}
	for _, v := range m.nonzero {
		nz[v] = true
	}
	// Representatives: the smallest identity-stored unit of each Unicode 3.2
	// class that the kernel also treats as combining.
	repOf := map[uint8]uint16{}
	for _, v := range m.nonzero {
		c := m.u.get(rune(v)).CCC
		if c == 0 {
			continue
		}
		if r, ok := repOf[c]; !ok || v < r {
			repOf[c] = v
		}
	}
	var reps []uint16
	for _, r := range repOf {
		reps = append(reps, r)
	}
	slices.Sort(reps)
	bodies = bodies[:0]
	type pair struct{ u, r uint16 }
	var pairs []pair
	for _, v := range m.nonzero {
		for _, r := range reps {
			if v == r {
				continue
			}
			pairs = append(pairs, pair{v, r})
			bodies = append(bodies, "a"+string(rune(v))+string(rune(r)), "a"+string(rune(r))+string(rune(v)))
		}
	}
	st, errs, err = prefixedBatch(m.work, "ccc2", 600, bodies)
	if err != nil {
		return err
	}
	for i, p := range pairs {
		if errs[2*i] != "" || errs[2*i+1] != "" {
			return fmt.Errorf("ccc pair %04X %04X failed", p.u, p.r)
		}
		s1, err := swapped(st[2*i], p.u, p.r)
		if err != nil {
			return err
		}
		s2, err := swapped(st[2*i+1], p.r, p.u)
		if err != nil {
			return err
		}
		rel := "?"
		switch {
		case s1 && !s2:
			rel = ">"
		case !s1 && s2:
			rel = "<"
		case !s1 && !s2:
			rel = "="
		}
		lines = append(lines, fmt.Sprintf("pair\t%04X\t%04X\t%s", p.u, p.r, rel))
	}
	return writeLines(filepath.Join(m.testdata, "ccc.tsv.gz"), lines)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// namePools are the character pools random test names are drawn from.
type namePools struct {
	base, marks, special []rune
}

func (m *measurement) pools() namePools {
	var p namePools
	add := func(dst *[]rune, lo, hi rune) {
		for r := lo; r <= hi; r++ {
			*dst = append(*dst, r)
		}
	}
	add(&p.base, 'a', 'z')
	add(&p.base, 'A', 'Z')
	add(&p.base, '0', '9')
	p.base = append(p.base, ' ', '.', '-', '_', ':', '%', '~', '!')
	add(&p.base, 0x00C0, 0x00FF)
	add(&p.base, 0x0100, 0x017F)
	add(&p.base, 0x0386, 0x03CE)
	add(&p.base, 0x0400, 0x045F)
	add(&p.base, 0x1E00, 0x1EF9)
	add(&p.base, 0x1F00, 0x1FFC)
	for i := 0; i < 64; i++ {
		p.base = append(p.base, 0xAC00+rune(i*173), 0x4E00+rune(i*331), 0x3040+rune(i))
	}
	for _, v := range m.nonzero {
		p.marks = append(p.marks, rune(v))
	}
	p.special = []rune{0x2400, 0x200B, 0x200C, 0x200D, 0x200E, 0x200F, 0x202A, 0x202E, 0x206A, 0x206F, 0xFEFF,
		0x2126, 0x212A, 0x212B, 0x2000, 0x2FFF, 0xF900, 0xFA2D, 0xFFFD, 0xFFFE, 0xFFFF, 0x1100, 0x1161, 0x11A8,
		0x10400, 0x10428, 0x1D15E, 0x1D165, 0x1D167, 0x1D16D, 0x20000, 0x2F800, 0x10FFFD, 0x0130, 0x0131, 0x017F,
		0x03C2, 0x03A3, 0x00DF, 0x01C4, 0x01C5, 0x01C6, 0x0340, 0x0344, 0x0F73, 0x0F81, 0x0958, 0xFB1D, 0xFB2A}
	return p
}

func (p namePools) random(rng *rand.Rand, maxLen int) string {
	n := 1 + rng.IntN(maxLen)
	var b []rune
	for i := 0; i < n; i++ {
		switch x := rng.IntN(100); {
		case x < 50 || len(p.marks) == 0:
			b = append(b, p.base[rng.IntN(len(p.base))])
		case x < 85:
			b = append(b, p.marks[rng.IntN(len(p.marks))])
		default:
			b = append(b, p.special[rng.IntN(len(p.special))])
		}
	}
	if b[0] == '.' {
		b[0] = 'o'
	}
	return string(b)
}

// measureSeq records the stored form of many composed inputs: every
// canonically decomposable BMP character followed by marks of several
// classes, targeted cases and random sequences.
func (m *measurement) measureSeq() error {
	var bodies []string
	follow := []rune{0x0316, 0x0334, 0x0345, 0x05B0, 0x031B}
	var decs []rune
	for r, c := range m.u.chars {
		if r <= 0xFFFF && c.Decomp != nil {
			decs = append(decs, r)
		}
	}
	slices.Sort(decs)
	for _, d := range decs {
		for _, f := range follow {
			s := string(d)
			if f != 0 {
				s += string(f)
			}
			bodies = append(bodies, s)
		}
	}
	targeted := []string{
		"a\u0301\u0316", "a\u0316\u0301", "\u00e9\u0316", "e\u0301\u0316", "\uac00\u11a8", "\u1100\u1161\u11a8",
		"a\u0301\U0001D167", "a\U0001D167\u0301", "a\u0301\U0001D165\u0316", "a\u0345\u0301\u0316\u0334",
		"a\u0334\u0345\u0301\u0316\u05b0\u0327", "\u1e69\u0323", "\u1e0b\u0323", "\u01d5\u0331", "\u0344\u0316",
		"a\u0360\u0301", "a\u035d\u0345", "a\u0362\u0360", "\u0f73\u0f71", "\u0f77", "\u0f79",
		"x\u2126", "x\u212b", "x\uf900", "x\U0001d15e", "x\U0002f800", "x\u200cy", "x:y", "x\u2400y",
	}
	bodies = append(bodies, targeted...)
	rng := rand.New(rand.NewPCG(1, 2))
	p := m.pools()
	for i := 0; i < 3000; i++ {
		bodies = append(bodies, p.random(rng, 10))
	}
	st, errs, err := prefixedBatch(m.work, "seq", 200, bodies)
	if err != nil {
		return err
	}
	var lines []string
	for i, b := range bodies {
		r := errs[i]
		if r == "" {
			r = fmtUnits(st[i])
		}
		lines = append(lines, fmtRunes(b)+"\t"+r)
	}
	return writeLines(filepath.Join(m.testdata, "seq.tsv.gz"), lines)
}

// orderEntry is one child of a measured directory, in on-disk key order:
// the name it was stored as, the input that created it and the inputs that
// then failed with EEXIST because they compare equal to it.
type orderEntry struct {
	stored    []uint16
	input     string
	colliders []string
}

func orderEntries(br *batchResult, inputs []string) ([]orderEntry, error) {
	created := map[uint32]int{}
	coll := map[uint32][]int{}
	for i, r := range br.Results {
		switch {
		case r.Errno == 0:
			created[r.Ino] = i
		case r.Errno == syscall.EEXIST && r.Existing:
			coll[r.Ino] = append(coll[r.Ino], i)
		default:
			return nil, fmt.Errorf("create %s: %s", fmtRunes(inputs[i]), errnoName(r.Errno))
		}
	}
	var out []orderEntry
	seen := map[uint32]bool{}
	for _, rec := range br.Order {
		seen[rec.CNID] = true
		i, ok := created[rec.CNID]
		if !ok {
			return nil, fmt.Errorf("on-disk child %s not created by us", fmtUnits(rec.Name))
		}
		e := orderEntry{stored: rec.Name, input: inputs[i]}
		for _, j := range coll[rec.CNID] {
			e.colliders = append(e.colliders, inputs[j])
		}
		out = append(out, e)
	}
	for ino := range coll {
		if !seen[ino] {
			return nil, fmt.Errorf("collision with unknown CNID %d", ino)
		}
	}
	return out, nil
}

// orderLines renders entries as "stored<TAB>input<TAB>collider..." lines.
func orderLines(es []orderEntry) []string {
	var lines []string
	for _, e := range es {
		l := fmtUnits(e.stored) + "\t" + fmtRunes(e.input)
		for _, c := range e.colliders {
			l += "\t" + fmtRunes(c)
		}
		lines = append(lines, l)
	}
	return lines
}

// measureFold creates pre+c+post for every storable unit c (and
// supplementary pairs), recording collisions (the kernel's case-insensitive
// equality) and the on-disk key order: "x"+c, and "a"+c+"b" after "ab" to
// find ignorables.
//
// The files use a compact format (see readFoldFile): per on-disk child the
// stored units between pre and post, then the inputs (runes, without pre and
// post) that collided with it; runs of single-unit children stored as their
// own input, consecutive in value and without colliders, become "R<TAB>first<TAB>last".
func (m *measurement) measureFold() error {
	var units []uint16
	for v := range m.storable {
		units = append(units, v)
	}
	slices.Sort(units)
	var supp []rune
	for h := rune(0xD800); h <= 0xDBFF; h++ {
		supp = append(supp, utf16.DecodeRune(h, 0xDC00))
	}
	for l := rune(0xDC01); l <= 0xDFFF; l++ {
		supp = append(supp, utf16.DecodeRune(0xD800, l))
	}
	var mids []string
	for _, v := range units {
		mids = append(mids, string(m.storable[v]))
	}
	for _, r := range supp {
		mids = append(mids, string(r))
	}
	for _, t := range []struct{ file, pre, post string }{{"fold.tsv.gz", "x", ""}, {"ign.tsv.gz", "a", "b"}} {
		names := []string{t.pre + t.post}
		for _, s := range mids {
			names = append(names, t.pre+s+t.post)
		}
		br, err := runBatch(m.work, "fold", "HFS+", 400, names)
		if err != nil {
			return err
		}
		es, err := orderEntries(br, names)
		if err != nil {
			return err
		}
		if t.pre == "x" {
			m.foldX = es
		}
		lines := []string{fmt.Sprintf("#kct %02X", br.KCT), "#pre " + fmtRunes(t.pre), "#post " + fmtRunes(t.post)}
		strip := func(s string) string { return strings.TrimSuffix(strings.TrimPrefix(s, t.pre), t.post) }
		for i := 0; i < len(es); i++ {
			e := es[i]
			mid, err := stripPrefix(e.stored, t.pre)
			if err != nil {
				return err
			}
			mid = mid[:len(mid)-len(t.post)]
			single := func(e orderEntry, mid []uint16) bool {
				return len(mid) == 1 && len(e.colliders) == 0 && strip(e.input) == string(rune(mid[0]))
			}
			if single(e, mid) {
				j := i
				for j+1 < len(es) {
					n, _ := stripPrefix(es[j+1].stored, t.pre)
					n = n[:len(n)-len(t.post)]
					if !single(es[j+1], n) || n[0] != mid[0]+uint16(j+1-i) {
						break
					}
					j++
				}
				if j > i {
					lines = append(lines, fmt.Sprintf("R\t%04X\t%04X", mid[0], mid[0]+uint16(j-i)))
					i = j
					continue
				}
			}
			l := fmtUnits(mid) + "\t" + fmtRunes(strip(e.input))
			for _, c := range e.colliders {
				l += "\t" + fmtRunes(strip(c))
			}
			lines = append(lines, l)
		}
		if err := writeLines(filepath.Join(m.testdata, t.file), lines); err != nil {
			return err
		}
	}
	return nil
}

// measureOrder creates random multi-unit names, deliberately including
// case and ignorable variants, on HFS+ and HFSX and records the key order.
func (m *measurement) measureOrder() error {
	rng := rand.New(rand.NewPCG(3, 4))
	p := m.pools()
	var names []string
	seen := map[string]bool{}
	addName := func(s string) {
		if !seen[s] && s != "" && s != "." && s != ".." && len(utf16.Encode([]rune(s))) <= 200 {
			seen[s] = true
			names = append(names, s)
		}
	}
	ign := []rune{0x200C, 0x200D, 0x200E, 0x202A, 0x206A, 0xFEFF}
	for i := 0; i < 3000; i++ {
		s := p.random(rng, 8)
		addName(s)
		switch rng.IntN(4) {
		case 0:
			addName(strings.ToUpper(s))
		case 1:
			r := []rune(s)
			k := rng.IntN(len(r) + 1)
			addName(string(r[:k]) + string(ign[rng.IntN(len(ign))]) + string(r[k:]))
		case 2:
			addName(s + string(rune('a'+rng.IntN(26))))
		}
	}
	for _, fs := range []struct{ file, fs string }{{"order.tsv.gz", "HFS+"}, {"hfsx.tsv.gz", "HFSX"}} {
		br, err := runBatch(m.work, "order", fs.fs, 100, names)
		if err != nil {
			return err
		}
		es, err := orderEntries(br, names)
		if err != nil {
			return err
		}
		lines := append([]string{fmt.Sprintf("#kct %02X", br.KCT)}, orderLines(es)...)
		if err := writeLines(filepath.Join(m.testdata, fs.file), lines); err != nil {
			return err
		}
	}
	return nil
}

// patchCatalog rewrites, in every index and leaf node of the catalog, each
// key (parentID, name) found in repl to the replacement name of equal length.
func patchCatalog(path string, repl map[string][]uint16) (int, error) {
	fh, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	vh := make([]byte, 512)
	if _, err := fh.ReadAt(vh, 1024); err != nil {
		return 0, err
	}
	bs := int64(binary.BigEndian.Uint32(vh[40:]))
	cf := &forkMap{bs, parseExtents(vh[0x110+16:])}
	h, err := readHeader(fh, cf)
	if err != nil {
		return 0, err
	}
	hb := make([]byte, 64)
	o0, _ := cf.offset(0)
	fh.ReadAt(hb, o0)
	total := binary.BigEndian.Uint32(hb[14+22:])
	patched := 0
	for n := uint32(1); n < total; n++ {
		nd, _, err := readNode(fh, cf, n, h.nodeSize)
		if err != nil {
			return patched, err
		}
		kind := int8(nd[8])
		if kind != -1 && kind != 0 {
			continue
		}
		nrec := int(binary.BigEndian.Uint16(nd[10:]))
		if nrec == 0 || nrec > h.nodeSize/8 {
			continue
		}
		dirty := false
		for i := 0; i < nrec; i++ {
			o := int(binary.BigEndian.Uint16(nd[h.nodeSize-2*(i+1):]))
			if o+8 > h.nodeSize {
				break
			}
			nl := int(binary.BigEndian.Uint16(nd[o+6:]))
			if o+8+2*nl > h.nodeSize {
				continue
			}
			k := string(nd[o+2:o+6]) + string(nd[o+8:o+8+2*nl])
			if r, ok := repl[k]; ok && len(r) == nl {
				for j, x := range r {
					binary.BigEndian.PutUint16(nd[o+8+2*j:], x)
				}
				dirty = true
				patched++
			}
		}
		if dirty {
			off := int64(n) * int64(h.nodeSize)
			for i := 0; i < h.nodeSize; {
				chunk := int(bs - (off+int64(i))%bs)
				if chunk > h.nodeSize-i {
					chunk = h.nodeSize - i
				}
				io, err := cf.offset(off + int64(i))
				if err != nil {
					return patched, err
				}
				if _, err := fh.WriteAt(nd[i:i+chunk], io); err != nil {
					return patched, err
				}
				i += chunk
			}
		}
	}
	return patched, nil
}

func keyString(pid uint32, name []uint16) string {
	b := make([]byte, 4+2*len(name))
	binary.BigEndian.PutUint32(b, pid)
	for i, x := range name {
		binary.BigEndian.PutUint16(b[4+2*i:], x)
	}
	return string(b)
}

func placeholder(n int) string { return strings.Repeat("Q", n) }

// patchedDirs makes one directory per case holding one file whose stored
// name is then patched to the case's units; it returns the image mounted
// read-only and each directory's path.
func (m *measurement) patchedDirs(name string, sizeMB int, cases [][]uint16) (*image, []string, error) {
	im, err := newImage(m.work, name, "HFS+", sizeMB)
	if err != nil {
		return nil, nil, err
	}
	dirs := make([]string, len(cases))
	repl := map[string][]uint16{}
	for i, c := range cases {
		dirs[i] = fmt.Sprintf("%s/%05X", im.mnt, i)
		id, err := mkdir(dirs[i])
		if err != nil {
			im.detach()
			return nil, nil, err
		}
		ph := placeholder(len(c))
		if r := createExcl(dirs[i] + "/" + ph); r.Errno != 0 {
			im.detach()
			return nil, nil, fmt.Errorf("placeholder: %s", errnoName(r.Errno))
		}
		repl[keyString(id, utf16.Encode([]rune(ph)))] = c
	}
	if err := im.detach(); err != nil {
		return nil, nil, err
	}
	n, err := patchCatalog(im.path, repl)
	if err != nil {
		return nil, nil, err
	}
	if n < len(cases) {
		return nil, nil, fmt.Errorf("patched only %d of %d keys", n, len(cases))
	}
	if err := im.attach(true); err != nil {
		return nil, nil, err
	}
	return im, dirs, nil
}

func lookupOK(path string) bool {
	var st syscall.Stat_t
	return syscall.Lstat(path, &st) == nil
}

// measurePresent patches stored names the kernel would never write itself
// and records how readdir presents them and whether that name looks up.
func (m *measurement) measurePresent() error {
	cases := [][]uint16{
		{0x0000}, {'a', 0x0000, 'b'}, {0x0000, 0x0000, 'H'}, {0xD800}, {0xDC00}, {'a', 0xD800, 'b'}, {'a', 0xDC00, 'b'},
		{0xDC00, 0xD800}, {0xDBFF, 0xDFFF}, {':'}, {'a', ':', 'b'}, {'/'}, {'a', '/', 'b'}, {0x2400}, {'a', 0x2400},
		{0xFFFE}, {0xFFFF}, {'a', 0xFFFE}, {'a', 0xFFFE, 'b'}, {'a', 0xFFFF, 'b'}, {0x00E9}, {'e', 0x0301}, {0x0301}, {0x0301, 'e'}, {'a', 0x0301, 0x0316},
		{0xAC00}, {0x1100, 0x1161}, {0x212B}, {'%', 'F', 'F'}, {0x0085}, {0x001F}, {0xF8FF},
	}
	im, dirs, err := m.patchedDirs("present", 32, cases)
	if err != nil {
		return err
	}
	defer im.remove()
	var lines []string
	for i, c := range cases {
		ents, err := os.ReadDir(dirs[i])
		res := ""
		switch {
		case err != nil:
			res = "!readdir:" + err.Error()
		case len(ents) != 1:
			res = fmt.Sprintf("!entries:%d", len(ents))
		default:
			n := ents[0].Name()
			res = fmt.Sprintf("% X", []byte(n))
			res = strings.ReplaceAll(res, " ", "")
			if lookupOK(dirs[i] + "/" + n) {
				res += "\tlookup=ok"
			} else {
				res += "\tlookup=fail"
			}
		}
		lines = append(lines, fmtUnits(c)+"\t"+res)
	}
	if err := im.detach(); err != nil {
		return err
	}
	return writeLines(filepath.Join(m.testdata, "present.tsv"), lines)
}

// measureLookup stores (by patching) "a"+u+"b" for every unit u the kernel
// never stores itself and checks, by lookups, whether u is ignorable and
// whether it folds equal to one of its Unicode case partners.
func (m *measurement) measureLookup() error {
	var us []uint16
	for r := rune(0); r <= 0xFFFF; r++ {
		if _, ok := m.storable[uint16(r)]; !ok {
			us = append(us, uint16(r))
		}
	}
	type probe struct {
		label string
		name  string
	}
	cases := make([][]uint16, len(us))
	probes := make([][]probe, len(us))
	for i, v := range us {
		cases[i] = []uint16{'a', v, 'b'}
		probes[i] = []probe{{"ab", "ab"}}
		c := m.u.get(rune(v))
		seen := map[rune]bool{}
		for _, cand := range []rune{c.Lower, c.Upper, c.Title} {
			if cand == 0 || cand > 0xFFFF || seen[cand] {
				continue
			}
			seen[cand] = true
			if in, ok := m.storable[uint16(cand)]; ok && in == cand {
				probes[i] = append(probes[i], probe{fmt.Sprintf("%04X", cand), "a" + string(cand) + "b"})
			}
		}
		switch v {
		case 0x2400, 0xFFFE, 0xFFFF:
			// is it folded like the stored NUL that U+2400 input becomes?
			probes[i] = append(probes[i], probe{"0000", "a\u2400b"})
		case ':':
			// like the '/' that ':' input becomes?
			probes[i] = append(probes[i], probe{"002F", "a:b"})
		}
	}
	im, dirs, err := m.patchedDirs("lookup", 200, cases)
	if err != nil {
		return err
	}
	defer im.remove()
	var lines []string
	for i, v := range us {
		l := fmt.Sprintf("%04X", v)
		for _, p := range probes[i] {
			l += fmt.Sprintf("\t%s=%d", p.label, b2i(lookupOK(dirs[i]+"/"+p.name)))
		}
		lines = append(lines, l)
	}
	if err := im.detach(); err != nil {
		return err
	}
	return writeLines(filepath.Join(m.testdata, "lookup.tsv.gz"), lines)
}

// measureFacts records assorted behaviour: key compare types, name length
// limits, the ':' swap and what the kernel does with invalid UTF-8.
func (m *measurement) measureFacts() error {
	var lines []string
	for _, fs := range []string{"HFS+", "HFSX", "Journaled HFS+", "Case-sensitive Journaled HFS+"} {
		br, err := runBatch(m.work, "facts", fs, 32, []string{"a"})
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("kct\t%s\t%02X", fs, br.KCT))
	}
	inputs := []struct{ label, name string }{
		{"colon", "a:b"},
		{"u2400", "n\u2400n"},
		{"ascii255", strings.Repeat("L", 255)},
		{"ascii256", strings.Repeat("M", 256)},
		{"e_acute127", strings.Repeat("\u00e9", 127)},
		{"e_acute128", strings.Repeat("\u00ea", 128)},
		{"hangul85", strings.Repeat("\uac01", 85)},
		{"hangul86", strings.Repeat("\uac02", 86)},
		{"supp127", strings.Repeat("\U00010400", 127)},
		{"supp128", strings.Repeat("\U00010401", 128)},
		{"ascii254_zwnj", strings.Repeat("N", 254) + "\u200c"},
		{"ascii255_zwnj", strings.Repeat("O", 255) + "\u200c"},
		{"invalid_ff", "bad\xff"},
		{"invalid_overlong", "ov\xc0\xaf"},
		{"invalid_surrogate", "sur\xed\xa0\x80"},
		{"invalid_truncated", "tr\xe9x"},
		{"invalid_toobig", "big\xf4\x90\x80\x80"},
		{"percent", "pc%FF"},
	}
	names := make([]string, len(inputs))
	for i, in := range inputs {
		names[i] = in.name
	}
	br, err := runBatch(m.work, "facts", "HFS+", 32, names)
	if err != nil {
		return err
	}
	for i, in := range inputs {
		r := br.Results[i]
		res := ""
		switch {
		case r.Errno == 0:
			res = "stored " + fmtUnits(br.Stored[i])
		default:
			res = "!" + errnoName(r.Errno)
		}
		lines = append(lines, fmt.Sprintf("create\t%s\t%s", in.label, res))
	}
	// The private metadata directory is named with four NULs. Is it visible
	// through the U+2400 spelling?
	im, err := newImage(m.work, "facts", "HFS+", 32)
	if err != nil {
		return err
	}
	priv := im.mnt + "/\u2400\u2400\u2400\u2400HFS+ Private Data"
	lines = append(lines, fmt.Sprintf("privdir_lookup\t%d", b2i(lookupOK(priv))))
	ents, _ := os.ReadDir(im.mnt)
	var en []string
	for _, e := range ents {
		en = append(en, fmtRunes(e.Name()))
	}
	sort.Strings(en)
	lines = append(lines, "root_readdir\t"+strings.Join(en, "\t"))
	im.detach()
	c, err := readCatalog(im.path)
	if err != nil {
		return err
	}
	var rn []string
	for _, r := range c.children(2) {
		rn = append(rn, fmtUnits(r.Name))
	}
	lines = append(lines, "root_ondisk\t"+strings.Join(rn, "\t"))
	im.remove()
	return writeLines(filepath.Join(m.testdata, "facts.tsv"), lines)
}
