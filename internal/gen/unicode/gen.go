package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// report collects findings; everything in it ends up in the generated
// file's header and on stderr.
type report struct {
	lines []string
	fails []string
}

func (r *report) addf(format string, a ...any) { r.lines = append(r.lines, fmt.Sprintf(format, a...)) }
func (r *report) failf(format string, a ...any) {
	if len(r.fails) < 50 {
		r.fails = append(r.fails, fmt.Sprintf(format, a...))
	} else if len(r.fails) == 50 {
		r.fails = append(r.fails, "...")
	}
}

// appleExcluded reports whether Apple leaves r undecomposed although Unicode
// 3.2 gives it a canonical decomposition (TN1150: U+2000-U+2FFF,
// U+F900-U+FAFF, U+2F800-U+2FAFF).
func appleExcluded(r rune) bool {
	return r >= 0x2000 && r <= 0x2FFF || r >= 0xF900 && r <= 0xFAFF || r >= 0x2F800 && r <= 0x2FAFF
}

// candFull is the full canonical decomposition of r by Unicode 3.2 with
// Apple's documented exclusions.
func candFull(u *ucd, r rune) []rune {
	if r >= sBase && r < sBase+sCount {
		var out []rune
		for _, x := range hangul(r) {
			out = append(out, rune(x))
		}
		return out
	}
	c := u.get(r)
	if c.Decomp == nil || appleExcluded(r) {
		return []rune{r}
	}
	var out []rune
	for _, d := range c.Decomp {
		out = append(out, candFull(u, d)...)
	}
	return out
}

// candStored is the stored form the Unicode 3.2 model predicts for one code
// point: full decomposition, canonical order by Unicode 3.2 classes, UTF-16.
func candStored(u *ucd, r rune) []uint16 {
	rs := candFull(u, r)
	for i := 1; i < len(rs); i++ {
		c := u.get(rs[i]).CCC
		if c == 0 {
			continue
		}
		for j := i; j > 0 && u.get(rs[j-1]).CCC > c; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
	return utf16Of(string(rs))
}

// testedCodePoints lists the code points the decomposition measurement
// covered: every BMP code point but surrogates and '/', plus the listed
// supplementary ones.
func testedCodePoints(m *measured) []rune {
	var out []rune
	for r := rune(1); r <= 0xFFFF; r++ {
		if r != '/' && (r < 0xD800 || r > 0xDFFF) {
			out = append(out, r)
		}
	}
	var supp []rune
	for r := range m.decomp {
		if r > 0xFFFF {
			supp = append(supp, r)
		}
	}
	for r := range m.derr {
		if r > 0xFFFF {
			supp = append(supp, r)
		}
	}
	slices.Sort(supp)
	return append(out, supp...)
}

func (m *measured) storedOf(r rune) []uint16 {
	if s, ok := m.decomp[r]; ok {
		return s
	}
	return []uint16{uint16(r)}
}

func generate(u *ucd, testdata, out string) error {
	m, err := loadMeasured(testdata)
	if err != nil {
		return err
	}
	rep := &report{}
	t := &tables{decomp: map[uint16][]uint16{}}

	buildDecomp(u, m, t, rep)
	buildCCC(u, m, t, rep)
	verifyConversion(m, t, rep)
	buildFold(u, m, t, rep)
	verifyFold(m, t, rep)
	verifyPresent(m, rep)

	for _, l := range rep.lines {
		fmt.Fprintln(os.Stderr, l)
	}
	if len(rep.fails) > 0 {
		for _, f := range rep.fails {
			fmt.Fprintln(os.Stderr, "FAIL:", f)
		}
		return fmt.Errorf("%d verification failures; %s not written", len(rep.fails), out)
	}
	lic, err := os.ReadFile(filepath.Join(testdata, "UNICODE-LICENSE.txt"))
	if err != nil {
		return err
	}
	src, err := emit(m, t, rep, string(lic))
	if err != nil {
		return err
	}
	return os.WriteFile(out, src, 0o644)
}

// buildDecomp takes the kernel's stored form of every code point as the
// truth, compares it with the Unicode 3.2 model and keeps the non-identity
// BMP mappings that code does not produce algorithmically.
func buildDecomp(u *ucd, m *measured, t *tables, rep *report) {
	var cand, same, diff, special int
	var diffs []string
	kinds := map[string]int{}
	for _, r := range testedCodePoints(m) {
		if e, ok := m.derr[r]; ok {
			rep.failf("decomp: U+%04X could not be created: %s", r, e)
			continue
		}
		got := m.storedOf(r)
		want := candStored(u, r)
		if len(want) != 1 || rune(want[0]) != r {
			cand++
		}
		switch {
		case r == ':':
			if !slices.Equal(got, []uint16{'/'}) {
				rep.failf("':' stored as %s", fmtUnits(got))
			}
			special++
			continue
		case r == 0x2400:
			if !slices.Equal(got, []uint16{0}) {
				rep.failf("U+2400 stored as %s", fmtUnits(got))
			}
			special++
			continue
		case slices.Equal(got, escapeRune(r)):
			t.escaped = append(t.escaped, r)
			special++
			continue
		}
		if slices.Equal(got, want) {
			same++
		} else {
			diff++
			k := "other"
			switch {
			case r > 0xFFFF && len(got) == 2 && slices.Equal(got, utf16Of(string(r))):
				k = "supplementary left undecomposed"
			}
			kinds[k]++
			if k == "other" {
				diffs = append(diffs, fmt.Sprintf("U+%04X kernel %s, Unicode 3.2 %s", r, fmtUnits(got), fmtUnits(want)))
			}
		}
		if r <= 0xFFFF && !(r >= sBase && r < sBase+sCount) && !(len(got) == 1 && rune(got[0]) == r) {
			t.decomp[uint16(r)] = got
		}
		if r >= sBase && r < sBase+sCount && !slices.Equal(got, hangul(r)) {
			rep.failf("Hangul U+%04X stored as %s, not algorithmically", r, fmtUnits(got))
		}
	}
	rep.addf("decomposition: %d code points measured; Unicode 3.2 (with Apple's exclusions U+2000-2FFF, U+F900-FAFF, U+2F800-2FAFF) decomposes %d of them;", len(testedCodePoints(m)), cand)
	rep.addf("  kernel agrees on %d, differs on %d; %d kernel-only conversions (':' -> '/', U+2400 -> NUL, %d escaped noncharacters)", same, diff, special, len(t.escaped))
	for k, n := range kinds {
		rep.addf("  difference: %s: %d", k, n)
	}
	for _, d := range diffs {
		rep.addf("  difference: %s", d)
	}
	rep.addf("  table: %d BMP decompositions (Hangul syllables algorithmic)", len(t.decomp))
}

// buildCCC derives the kernel's combining classes from the probe and pair
// measurements, numbering them with the Unicode 3.2 class values.
func buildCCC(u *ucd, m *measured, t *tables, rep *report) {
	repsSet := map[uint16]bool{}
	for _, p := range m.pairs {
		repsSet[p.r] = true
	}
	cls := func(v uint16) uint8 { return u.get(rune(v)).CCC }
	rel := func(a, b uint8) byte {
		switch {
		case a < b:
			return '<'
		case a > b:
			return '>'
		}
		return '='
	}
	// Representatives must be ordered as Unicode 3.2 orders them.
	for _, p := range m.pairs {
		if repsSet[p.u] && p.rel != rel(cls(p.u), cls(p.r)) {
			rep.failf("ccc: kernel orders reps U+%04X %c U+%04X, Unicode 3.2 classes %d, %d", p.u, p.rel, p.r, cls(p.u), cls(p.r))
		}
	}
	byU := map[uint16][]cccPair{}
	for _, p := range m.pairs {
		byU[p.u] = append(byU[p.u], p)
	}
	var agree, differ int
	for v := range m.nz {
		ps := byU[v]
		var eq []uint8
		lo, hi := 0, 256
		for _, p := range ps {
			c := int(cls(p.r))
			switch p.rel {
			case '=':
				eq = append(eq, uint8(c))
			case '>':
				lo = max(lo, c)
			case '<':
				hi = min(hi, c)
			default:
				rep.failf("ccc: inconsistent reordering of U+%04X and U+%04X", p.u, p.r)
			}
		}
		if repsSet[v] {
			eq = append(eq, cls(v))
		}
		var k uint8
		switch {
		case len(eq) > 0:
			k = eq[0]
			for _, e := range eq {
				if e != k {
					rep.failf("ccc: U+%04X equal to reps of classes %d and %d", v, k, e)
				}
			}
		case int(cls(v)) > lo && int(cls(v)) < hi:
			k = cls(v)
		case lo+1 < hi:
			k = uint8(lo + 1)
		default:
			rep.failf("ccc: no class for U+%04X between %d and %d", v, lo, hi)
		}
		for _, p := range ps {
			if rel(k, cls(p.r)) != p.rel {
				rep.failf("ccc: class %d for U+%04X contradicts U+%04X %c", k, v, p.r, p.rel)
			}
		}
		t.ccc[v] = k
		if k == cls(v) {
			agree++
		} else {
			differ++
			rep.addf("  combining class: U+%04X kernel %d, Unicode 3.2 %d", v, k, cls(v))
		}
	}
	// Units the probes found non-combining, but Unicode 3.2 gives a class.
	var zeroed []string
	for v := 0; v <= 0xFFFF; v++ {
		r := rune(v)
		if _, nz := m.nz[uint16(v)]; nz || cls(uint16(v)) == 0 {
			continue
		}
		if s := m.storedOf(r); len(s) == 1 && s[0] == uint16(v) && (r < 0xD800 || r > 0xDFFF) {
			zeroed = append(zeroed, fmt.Sprintf("U+%04X(%d)", v, cls(uint16(v))))
		}
	}
	rep.addf("combining classes: %d units reordered by the kernel (%d representatives, probes U+%04X/U+%04X); %d agree with Unicode 3.2, %d differ; %d Unicode 3.2 marks the kernel does not reorder %s",
		len(m.nz), len(repsSet), m.probeHi, m.probeLo, agree, differ, len(zeroed), strings.Join(zeroed, " "))
}

// verifyConversion checks the model conversion against every measured stored name.
func verifyConversion(m *measured, t *tables, rep *report) {
	n := 0
	check := func(what, in string, want []uint16) {
		n++
		got, err := t.toHFS(in)
		if err != nil {
			rep.failf("%s: toHFS(%s): %v, kernel stored %s", what, fmtRunes(in), err, fmtUnits(want))
		} else if !slices.Equal(got, want) {
			rep.failf("%s: toHFS(%s) = %s, kernel stored %s", what, fmtRunes(in), fmtUnits(got), fmtUnits(want))
		}
	}
	for _, r := range testedCodePoints(m) {
		if _, bad := m.derr[r]; !bad {
			check("decomp", string(r), m.storedOf(r))
		}
	}
	for _, c := range m.seq {
		if c.err != "" {
			if _, err := t.toHFS(c.in); err == nil || c.err != "!ENAMETOOLONG" {
				rep.failf("seq: %s kernel %s", fmtRunes(c.in), c.err)
			}
			continue
		}
		check("seq", c.in, c.out)
	}
	for _, f := range []orderFile{m.order, m.hfsx} {
		for _, e := range f.entries {
			check("order", e.input, e.stored)
		}
	}
	for _, l := range m.facts {
		if l[0] != "create" || strings.HasPrefix(l[1], "invalid") {
			continue
		}
		in := factInput(l[1])
		switch {
		case strings.HasPrefix(l[2], "stored "):
			want, _ := parseUnits(strings.TrimPrefix(l[2], "stored "))
			check("fact "+l[1], in, want)
		case l[2] == "!ENAMETOOLONG":
			n++
			if _, err := t.toHFS(in); err == nil {
				rep.failf("fact %s: kernel ENAMETOOLONG, model accepts", l[1])
			}
		}
	}
	rep.addf("conversion: model reproduces the kernel's stored form for %d measured inputs", n)
}

// factInput rebuilds the inputs of measureFacts by label.
func factInput(label string) string {
	rep := func(s string, n int) string { return strings.Repeat(s, n) }
	switch label {
	case "colon":
		return "a:b"
	case "u2400":
		return "n\u2400n"
	case "ascii255":
		return rep("L", 255)
	case "ascii256":
		return rep("M", 256)
	case "e_acute127":
		return rep("\u00e9", 127)
	case "e_acute128":
		return rep("\u00ea", 128)
	case "hangul85":
		return rep("\uac01", 85)
	case "hangul86":
		return rep("\uac02", 86)
	case "supp127":
		return rep("\U00010400", 127)
	case "supp128":
		return rep("\U00010401", 128)
	case "ascii254_zwnj":
		return rep("N", 254) + "\u200c"
	case "ascii255_zwnj":
		return rep("O", 255) + "\u200c"
	case "percent":
		return "pc%FF"
	}
	return ""
}

// buildFold derives the case-folding table from the measured equivalence
// classes, ignorables and key order, naming each class's value with Unicode
// 3.2 simple lowercase mappings.
func buildFold(u *ucd, m *measured, t *tables, rep *report) {
	for i := range t.fold {
		t.fold[i] = uint16(i)
	}
	stored := func(in string) []uint16 {
		s, err := t.toHFS(in)
		if err != nil {
			rep.failf("fold: toHFS(%s): %v", fmtRunes(in), err)
		}
		return s
	}
	// Ignorables: the inputs that collided with the empty middle.
	ignorable := map[uint16]bool{}
	for fi, f := range []foldFile{m.fold, m.ign} {
		set := map[uint16]bool{}
		for _, e := range f.entries {
			if len(e.mid) == 0 {
				for _, c := range e.colliders {
					s := stored(c)
					if len(s) != 1 {
						rep.failf("fold: multi-unit ignorable %s", fmtUnits(s))
						continue
					}
					set[s[0]] = true
				}
			}
		}
		if fi == 0 {
			ignorable = set
		} else if len(set) != len(ignorable) {
			rep.failf("fold: ignorables differ between x+c (%d) and a+c+b (%d)", len(ignorable), len(set))
		} else {
			for v := range set {
				if !ignorable[v] {
					rep.failf("fold: U+%04X ignorable only in a+c+b", v)
				}
			}
		}
	}
	for v := range ignorable {
		t.fold[v] = 0
	}
	storable := map[uint16]bool{}
	type class struct{ members []uint16 }
	var classes []class
	for _, e := range m.fold.entries {
		if len(e.mid) != 1 {
			continue
		}
		c := class{members: []uint16{e.mid[0]}}
		for _, x := range e.colliders {
			s := stored(x)
			if len(s) != 1 {
				rep.failf("fold: U+%04X collided with multi-unit %s", e.mid[0], fmtUnits(s))
				continue
			}
			c.members = append(c.members, s[0])
		}
		for _, v := range c.members {
			storable[v] = true
		}
		classes = append(classes, c)
	}
	for v := range ignorable {
		storable[v] = true
	}
	lower := func(v uint16) uint16 {
		if l := u.get(rune(v)).Lower; l != 0 && l <= 0xFFFF {
			return uint16(l)
		}
		return v
	}
	// Pass 1: classes whose value is unambiguous.
	value := make([]int, len(classes)) // -1: undecided
	var multi int
	for i, c := range classes {
		value[i] = -1
		lows := map[uint16]bool{}
		for _, v := range c.members {
			lows[lower(v)] = true
		}
		if len(c.members) > 1 {
			multi++
		}
		var inClass []uint16
		for l := range lows {
			if slices.Contains(c.members, l) {
				inClass = append(inClass, l)
			}
		}
		switch {
		case len(lows) == 1 && len(inClass) == 1:
			value[i] = int(inClass[0])
		case len(lows) == 1 && !storable[lower(c.members[0])]:
			// e.g. U+212B ANGSTROM SIGN -> U+00E5, which is never stored; decide by order.
		case len(inClass) == 1:
			value[i] = int(inClass[0])
		case len(c.members) == 1:
			value[i] = int(c.members[0])
		default:
			// No Unicode 3.2 case relation names the class (Georgian capitals
			// U+10A0..U+10C5 with U+10D0..U+10F5); decide by order.
		}
		if value[i] == 0 {
			value[i] = -1 // the stored NUL is not ignorable; decide by order
		}
	}
	var byOrder []byOrderClass
	// Pass 2: the rest take the first candidate that fits the kernel's order
	// between their neighbours: a member, the Unicode lowercase, 0xFFFF.
	for i, c := range classes {
		if value[i] >= 0 {
			continue
		}
		lo, hi := 0, 0x10000
		for j := i - 1; j >= 0; j-- {
			if value[j] >= 0 {
				lo = value[j]
				break
			}
		}
		for j := i + 1; j < len(classes); j++ {
			if value[j] >= 0 {
				hi = value[j]
				break
			}
		}
		var cands []int
		for _, v := range slices.Sorted(slices.Values(c.members)) {
			cands = append(cands, int(v))
		}
		for _, cand := range append(cands, int(lower(c.members[0])), 0xFFFF) {
			if cand > lo && cand < hi {
				value[i] = cand
				break
			}
		}
		if value[i] < 0 {
			rep.failf("fold: no value for class %v between %04X and %04X", c.members, lo, hi)
			continue
		}
		if value[i] != int(c.members[0]) {
			byOrder = append(byOrder, byOrderClass{c.members, value[i]})
		}
	}
	orderMsg := fmt.Sprintf("  %d classes Unicode 3.2 does not name take their value from the kernel's order: %s", len(byOrder), fmtByOrder(byOrder))
	for i, c := range classes {
		if value[i] < 0 {
			continue
		}
		for _, v := range c.members {
			t.fold[v] = uint16(value[i])
		}
	}
	// Units the kernel never stores itself: lookups on patched catalogs.
	values := map[uint16]bool{}
	for i := range classes {
		if value[i] >= 0 {
			values[uint16(value[i])] = true
		}
	}
	probes := map[uint16][]probeCase{}
	for _, p := range m.probe {
		probes[p.u] = append(probes[p.u], p)
	}
	// fits reports whether folding u to f agrees with every probe of u.
	fits := func(u, f uint16) bool {
		for _, p := range probes[u] {
			for _, r := range p.rels {
				k := t.fold[r.k]
				want := byte('=')
				if k < f {
					want = '<'
				} else if k > f {
					want = '>'
				}
				if want != r.rel {
					return false
				}
			}
		}
		return true
	}
	var unstIgn, unstEq, unstLower, unstProbed, unstSameGap, unstRefuted int
	for _, c := range m.lookup {
		if storable[c.u] {
			rep.failf("fold: lookup case U+%04X is storable", c.u)
			continue
		}
		v := c.u
		var f uint16 = v
		decided := false
		for _, p := range c.probes {
			if !p.found {
				continue
			}
			decided = true
			if p.label == "ab" {
				f = 0
				unstIgn++
			} else {
				var x uint16
				fmt.Sscanf(p.label, "%04X", &x)
				f = t.fold[x]
				unstEq++
			}
		}
		if !decided {
			// Kernel-written names never hold these units, so only their
			// position among the stored fold values is observable (by the leaf
			// probes). Where the probes can tell identity from the Unicode 3.2
			// lowercase apart they have always chosen identity, so identity is
			// preferred and the lowercase kept only if the probes demand it.
			l := lower(v)
			hasLower := l != v && !storable[l]
			switch {
			case fits(v, v):
				if hasLower && len(probes[v]) > 0 {
					if fits(v, l) {
						unstSameGap++
					} else {
						unstRefuted++
					}
				}
			case hasLower && fits(v, l):
				f = l
				unstLower++
			default:
				rep.failf("fold: neither U+%04X nor its lowercase fits the leaf probes %v", v, probes[v])
			}
			if len(probes[v]) > 0 {
				unstProbed++
			}
			if values[f] {
				rep.failf("fold: U+%04X would fold to %04X, a measured class value, but did not look up equal", v, f)
			}
		}
		t.fold[v] = f
	}
	probeMsg := fmt.Sprintf("  fold of units never stored: %d located by leaf probes; they fold to themselves: of those with a Unicode 3.2 lowercase, the probes refute it for %d and cannot tell it from identity for %d (no stored value between); %d need their lowercase",
		unstProbed, unstRefuted, unstSameGap, unstLower)
	nIgn := 0
	for _, f := range t.fold {
		if f == 0 {
			nIgn++
		}
	}
	nonID := 0
	for i, f := range t.fold {
		if f != uint16(i) {
			nonID++
		}
	}
	rep.addf("case folding: %d storable units measured in %d classes (%d with more than one member), %d ignorable %s;",
		len(storable), len(classes), multi, len(ignorable), fmtUnits(sortedKeys(ignorable)))
	rep.addf("  %d units the kernel never stores, checked by lookup on patched catalogs: %d ignorable, %d equal to a measured class (U+FFFF like the stored NUL);",
		len(m.lookup), unstIgn, unstEq)
	rep.addf("%s", orderMsg)
	rep.addf("%s", probeMsg)
	rep.addf("  table: %d units fold to another value, %d to 0 (ignorable)", nonID, nIgn)
}

type byOrderClass struct {
	members []uint16
	value   int
}

// fmtByOrder lists classes and values, folding runs such as the Georgian
// {U+10A0+i, U+10D0+i} -> U+10D0+i into one range.
func fmtByOrder(cs []byOrderClass) string {
	var parts []string
	for i := 0; i < len(cs); {
		j := i
		for j+1 < len(cs) && len(cs[j+1].members) == len(cs[i].members) && cs[j+1].value == cs[j].value+1 &&
			slices.Equal(addUnits(cs[j].members, 1), cs[j+1].members) {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("{%s}..{%s} -> %04X..%04X", fmtUnits(cs[i].members), fmtUnits(cs[j].members), cs[i].value, cs[j].value))
		} else {
			parts = append(parts, fmt.Sprintf("{%s} -> %04X", fmtUnits(cs[i].members), cs[i].value))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

func addUnits(u []uint16, d uint16) []uint16 {
	out := make([]uint16, len(u))
	for i, x := range u {
		out[i] = x + d
	}
	return out
}

func sortedKeys(m map[uint16]bool) []uint16 {
	var out []uint16
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// verifyFold checks the fold table against every measured order and collision.
func verifyFold(m *measured, t *tables, rep *report) {
	nOrder, nColl := 0, 0
	for _, f := range []foldFile{m.fold, m.ign} {
		if f.kct != 0xCF {
			rep.failf("fold: key compare type %02X", f.kct)
		}
		pre, post := utf16Of(f.pre), utf16Of(f.post)
		full := func(mid []uint16) []uint16 { return slices.Concat(pre, mid, post) }
		for i, e := range f.entries {
			if i > 0 {
				nOrder++
				if a, b := full(f.entries[i-1].mid), full(e.mid); t.compare(a, b) >= 0 {
					rep.failf("fold order: %s then %s on disk, compare says %d", fmtUnits(a), fmtUnits(b), t.compare(a, b))
				}
			}
			for _, c := range e.colliders {
				nColl++
				s, _ := t.toHFS(f.pre + c + f.post)
				if t.compare(s, full(e.mid)) != 0 {
					rep.failf("fold collision: %s collided with %s, compare says %d", fmtUnits(s), fmtUnits(full(e.mid)), t.compare(s, full(e.mid)))
				}
			}
		}
	}
	for _, f := range []struct {
		o      orderFile
		kct    uint8
		cmp    func(a, b []uint16) int
		folded bool
	}{{m.order, 0xCF, t.compare, true}, {m.hfsx, 0xBC, compareBinary, false}} {
		if f.o.kct != f.kct {
			rep.failf("order: key compare type %02X, want %02X", f.o.kct, f.kct)
		}
		for i, e := range f.o.entries {
			if i > 0 {
				nOrder++
				if a, b := f.o.entries[i-1].stored, e.stored; f.cmp(a, b) >= 0 {
					rep.failf("order (%02X): %s then %s on disk, compare says %d", f.kct, fmtUnits(a), fmtUnits(b), f.cmp(a, b))
				}
			}
			for _, c := range e.colliders {
				nColl++
				s, _ := t.toHFS(c)
				if f.cmp(s, e.stored) != 0 {
					rep.failf("collision (%02X): %s collided with %s", f.kct, fmtUnits(s), fmtUnits(e.stored))
				}
			}
		}
	}
	nLook := 0
	for _, c := range m.lookup {
		name := []uint16{'a', c.u, 'b'}
		for _, p := range c.probes {
			nLook++
			var probe []uint16
			if p.label == "ab" {
				probe = []uint16{'a', 'b'}
			} else {
				var x uint16
				fmt.Sscanf(p.label, "%04X", &x)
				probe = []uint16{'a', x, 'b'}
			}
			if (t.compare(name, probe) == 0) != p.found {
				rep.failf("lookup: stored %s, lookup %s found=%v, compare %d", fmtUnits(name), fmtUnits(probe), p.found, t.compare(name, probe))
			}
		}
	}
	nProbe := 0
	for _, p := range m.probe {
		for _, r := range p.rels {
			nProbe++
			a, b := []uint16{p.prefix, r.k}, []uint16{p.prefix, p.u}
			got := t.compare(a, b)
			if !(got < 0 && r.rel == '<' || got == 0 && r.rel == '=' || got > 0 && r.rel == '>') {
				rep.failf("probe: kernel %s %c %s, compare says %d", fmtUnits(a), r.rel, fmtUnits(b), got)
			}
		}
	}
	nLook += nProbe
	rep.addf("comparison: %d adjacent on-disk key pairs in kernel order, %d kernel collisions and %d lookups (%d of them leaf probes) reproduced exactly (FastUnicodeCompare on HFS+ 0xCF, binary on HFSX 0xBC)", nOrder, nColl, nLook, nProbe)
}

// verifyPresent compares nameFromHFS's model with readdir of patched names.
func verifyPresent(m *measured, rep *report) {
	var same, differ []string
	for _, c := range m.present {
		got := fromHFS(c.stored)
		if c.err == "" && got == string(c.name) {
			same = append(same, fmtUnits(c.stored))
		} else {
			differ = append(differ, fmt.Sprintf("%s: kernel %+q%s, nameFromHFS %+q", fmtUnits(c.stored), c.name, c.err, got))
		}
	}
	rep.addf("presentation: nameFromHFS equals readdir for %d of %d patched names; differences (by design):", len(same), len(m.present))
	sort.Strings(differ)
	for _, d := range differ {
		rep.addf("  %s", d)
	}
}
