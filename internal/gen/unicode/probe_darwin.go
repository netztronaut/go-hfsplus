//go:build darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"syscall"
)

// measureProbe locates, relative to the fold values of stored names, the
// fold value of units the kernel never stores itself (precomposed letters,
// lone surrogates, ...), which no name created through POSIX can show.
//
// The kernel searches a B-tree node by binary search whose first probe is
// record (n-1)/2. A directory holds prefix+c for many storable units c; in
// chosen leaf nodes the middle record is patched to prefix+u. Looking up each
// other record of that node then compares it with the patched key first: it
// is found only if the kernel's comparison sends the search the right way, so
// every lookup yields the sign of compare(record, prefix+u) exactly (a hit on
// the patched file's inode means equal).
func (m *measurement) measureProbe() error {
	type rep struct {
		unit  uint16
		input rune
		val   int
	}
	lowerOf := func(v uint16) uint16 {
		if l := m.u.get(rune(v)).Lower; l != 0 && l <= 0xFFFF {
			return uint16(l)
		}
		return v
	}
	var reps []rep
	for _, e := range m.foldX {
		if len(e.stored) != 2 {
			continue // "x" itself and supplementary pairs
		}
		members := []uint16{e.stored[1]}
		for _, c := range e.colliders {
			if s := m.decomp[[]rune(c)[1]]; len(s) == 1 {
				members = append(members, s[0])
			}
		}
		val := int(slices.Max(members))
		for _, x := range members {
			if l := lowerOf(x); l != x && slices.Contains(members, l) {
				val = int(l)
			}
		}
		if members[0] == 0 {
			val = 0xFFFF
		}
		reps = append(reps, rep{e.stored[1], []rune(e.input)[1], val})
	}
	sort.Slice(reps, func(i, j int) bool { return reps[i].val < reps[j].val })
	vals := make([]int, len(reps))
	for i, r := range reps {
		vals[i] = r.val
	}

	type test struct {
		u   uint16
		hyp []int
	}
	var tests []test
	for v := 1; v <= 0xFFFF; v++ {
		u := uint16(v)
		if _, ok := m.storable[u]; ok {
			continue
		}
		switch {
		case v >= sBase && v < sBase+sCount && (v-sBase)%97 != 0:
			continue
		case v >= 0xD800 && v <= 0xDFFF && v%61 != 0:
			continue
		}
		c := m.u.get(rune(v))
		hyp := []int{v}
		for _, h := range []rune{c.Lower, c.Upper, c.Title} {
			if h != 0 && h <= 0xFFFF && !slices.Contains(hyp, int(h)) {
				hyp = append(hyp, int(h))
			}
		}
		tests = append(tests, test{u, hyp})
	}
	// The reps near any hypothesis, and 36 prefixed copies of them.
	near := map[int]bool{}
	for _, t := range tests {
		for _, h := range t.hyp {
			i := sort.SearchInts(vals, h)
			for j := max(0, i-12); j < min(len(reps), i+12); j++ {
				near[j] = true
			}
		}
	}
	var idx []int
	for j := range near {
		idx = append(idx, j)
	}
	slices.Sort(idx)
	prefixes := []rune("0123456789abcdefghijklmnopqrstuvwxyz")
	var names []string
	for _, p := range prefixes {
		for _, j := range idx {
			names = append(names, string(p)+string(reps[j].input))
		}
	}
	pending := make([]int, len(tests))
	for i := range pending {
		pending[i] = i
	}
	var lines []string
	for round := 0; len(pending) > 0 && round < 12; round++ {
		im, err := newImage(m.work, "probe", "HFS+", 400)
		if err != nil {
			return err
		}
		dirID, err := mkdir(im.mnt + "/d")
		if err != nil {
			im.detach()
			return err
		}
		fmt.Fprintf(os.Stderr, "probe round %d: creating %d names for %d tests\n", round, len(names), len(pending))
		for _, r := range createAll(im.mnt+"/d", names) {
			if r.Errno != 0 {
				im.detach()
				return fmt.Errorf("probe create: %s", errnoName(r.Errno))
			}
		}
		if err := im.detach(); err != nil {
			return err
		}
		cat, err := readCatalog(im.path)
		if err != nil {
			return err
		}
		repVal := map[uint16]int{}
		for _, r := range reps {
			repVal[r.unit] = r.val
		}
		type slot struct {
			mid     catRecord
			prefix  uint16
			recs    []catRecord
			lo, hi  int
			used    bool
			testIdx int
		}
		byNode := map[uint32][]catRecord{}
		var nodes []uint32
		for _, r := range cat.Records {
			if _, ok := byNode[r.Node]; !ok {
				nodes = append(nodes, r.Node)
			}
			byNode[r.Node] = append(byNode[r.Node], r)
		}
		var slots []*slot
		for _, n := range nodes {
			rs := byNode[n]
			mid := rs[(len(rs)-1)>>1]
			if mid.ParentID != dirID || mid.Type != 2 || len(mid.Name) != 2 {
				continue
			}
			s := &slot{mid: mid, prefix: mid.Name[0], lo: 1 << 20, hi: -1}
			before, after := 0, 0
			for _, r := range rs {
				if r.ParentID != dirID || r.Type != 2 || len(r.Name) != 2 || r.Name[0] != s.prefix || r.Index == mid.Index {
					continue
				}
				s.recs = append(s.recs, r)
				v := repVal[r.Name[1]]
				s.lo, s.hi = min(s.lo, v), max(s.hi, v)
				if r.Index < mid.Index {
					before++
				} else {
					after++
				}
			}
			if before > 0 && after > 0 {
				slots = append(slots, s)
			}
		}
		// Assign each test the free slot with the narrowest range spanning all its
		// hypotheses, else one spanning its first hypothesis (the unit itself).
		repl := map[string][]uint16{}
		var assigned []*slot
		var next []int
		for _, ti := range pending {
			t := tests[ti]
			lo, hi := slices.Min(t.hyp), slices.Max(t.hyp)
			var best *slot
			for pass := 0; pass < 2 && best == nil; pass++ {
				for _, s := range slots {
					if s.used || !(s.lo < lo && s.hi > hi) {
						continue
					}
					if best == nil || s.hi-s.lo < best.hi-best.lo {
						best = s
					}
				}
				lo, hi = t.hyp[0], t.hyp[0]
			}
			if best == nil {
				next = append(next, ti)
				continue
			}
			best.used, best.testIdx = true, ti
			repl[keyString(dirID, best.mid.Name)] = []uint16{best.prefix, t.u}
			assigned = append(assigned, best)
		}
		if _, err := patchCatalog(im.path, repl); err != nil {
			return err
		}
		if err := im.attach(true); err != nil {
			return err
		}
		inputOf := map[uint16]rune{}
		for _, r := range reps {
			inputOf[r.unit] = r.input
		}
		for _, s := range assigned {
			t := tests[s.testIdx]
			l := fmt.Sprintf("%04X\t%04X\t", t.u, s.prefix)
			for i, r := range s.recs {
				var st syscall.Stat_t
				p := filepath.Join(im.mnt, "d", string(rune(s.prefix))+string(inputOf[r.Name[1]]))
				found := syscall.Lstat(p, &st) == nil
				rel := byte('<')
				switch {
				case found && uint32(st.Ino) == s.mid.CNID:
					rel = '='
				case found && uint32(st.Ino) != r.CNID:
					return fmt.Errorf("probe: %s found CNID %d", p, st.Ino)
				case found != (r.Index < s.mid.Index):
					rel = '>'
				}
				if i > 0 {
					l += " "
				}
				l += fmt.Sprintf("%04X%c", r.Name[1], rel)
			}
			lines = append(lines, l)
		}
		if err := im.detach(); err != nil {
			return err
		}
		im.remove()
		if len(next) == len(pending) {
			break
		}
		pending = next

	}
	lines = append([]string{fmt.Sprintf("# %d tests, %d without a slot", len(tests), len(pending))}, lines...)
	return writeLines(filepath.Join(m.testdata, "probe.tsv.gz"), lines)
}
