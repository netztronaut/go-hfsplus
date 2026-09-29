package main

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// measured is everything read back from testdata/.
type measured struct {
	meta    map[string]string
	decomp  map[rune][]uint16 // listed code points (BMP non-identity, tested supplementary)
	derr    map[rune]string   // code points whose create failed
	nz      map[uint16]string // units the kernel reorders -> probe bits
	probeHi uint16
	probeLo uint16
	pairs   []cccPair
	seq     []seqCase
	fold    foldFile
	ign     foldFile
	order   orderFile
	hfsx    orderFile
	present []presentCase
	lookup  []lookupCase
	probe   []probeCase
	facts   [][]string
}

type cccPair struct {
	u, r uint16
	rel  byte // '<', '=', '>': kernel class of u relative to r
}

type seqCase struct {
	in  string
	out []uint16
	err string
}

type foldEntry struct {
	mid       []uint16 // stored units between pre and post
	input     string   // input between pre and post
	colliders []string // inputs between pre and post
}

type foldFile struct {
	kct       uint8
	pre, post string
	entries   []foldEntry
}

type orderEntryM struct {
	stored    []uint16
	input     string
	colliders []string
}

type orderFile struct {
	kct     uint8
	entries []orderEntryM
}

type presentCase struct {
	stored []uint16
	name   []byte // readdir name
	err    string
	lookup bool
}

type lookupCase struct {
	u      uint16
	probes []lookupProbe
}

// probeCase is one leaf probe: the kernel's comparison of prefix+k with the
// patched prefix+u for each other record k of the node.
type probeCase struct {
	u, prefix uint16
	rels      []probeRel
}

type probeRel struct {
	k   uint16
	rel byte // sign of compare(prefix+k, prefix+u)
}

type lookupProbe struct {
	label string // "ab" or a unit in hex
	found bool
}

func loadMeasured(dir string) (*measured, error) {
	m := &measured{meta: map[string]string{}, decomp: map[rune][]uint16{}, derr: map[rune]string{}, nz: map[uint16]string{}}
	p := func(name string) string { return filepath.Join(dir, name) }
	lines, err := readLines(p("meta.tsv"))
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		if len(l) == 2 {
			m.meta[l[0]] = l[1]
		}
	}

	if lines, err = readLines(p("decomp.tsv.gz")); err != nil {
		return nil, err
	}
	for _, l := range lines {
		cp, err := strconv.ParseUint(l[0], 16, 32)
		if err != nil || len(l) != 2 {
			return nil, fmt.Errorf("decomp: bad line %q", l)
		}
		if strings.HasPrefix(l[1], "!") {
			m.derr[rune(cp)] = l[1]
			continue
		}
		u, err := parseUnits(l[1])
		if err != nil {
			return nil, fmt.Errorf("decomp: %v", err)
		}
		m.decomp[rune(cp)] = u
	}

	if lines, err = readLines(p("ccc.tsv.gz")); err != nil {
		return nil, err
	}
	for _, l := range lines {
		h := func(s string) uint16 { v, _ := strconv.ParseUint(s, 16, 16); return uint16(v) }
		switch l[0] {
		case "high":
			m.probeHi = h(l[1])
		case "low":
			m.probeLo = h(l[1])
		case "nz":
			m.nz[h(l[1])] = l[2]
		case "pair":
			m.pairs = append(m.pairs, cccPair{h(l[1]), h(l[2]), l[3][0]})
		}
	}

	if lines, err = readLines(p("seq.tsv.gz")); err != nil {
		return nil, err
	}
	for _, l := range lines {
		in, err := parseRunes(l[0])
		if err != nil {
			return nil, err
		}
		c := seqCase{in: in}
		if strings.HasPrefix(l[1], "!") {
			c.err = l[1]
		} else if c.out, err = parseUnits(l[1]); err != nil {
			return nil, err
		}
		m.seq = append(m.seq, c)
	}

	if m.fold, err = readFoldFile(p("fold.tsv.gz")); err != nil {
		return nil, err
	}
	if m.ign, err = readFoldFile(p("ign.tsv.gz")); err != nil {
		return nil, err
	}
	if m.order, err = readOrderFile(p("order.tsv.gz")); err != nil {
		return nil, err
	}
	if m.hfsx, err = readOrderFile(p("hfsx.tsv.gz")); err != nil {
		return nil, err
	}

	if lines, err = readLines(p("present.tsv")); err != nil {
		return nil, err
	}
	for _, l := range lines {
		st, err := parseUnits(l[0])
		if err != nil {
			return nil, err
		}
		c := presentCase{stored: st}
		if strings.HasPrefix(l[1], "!") {
			c.err = l[1]
		} else {
			for i := 0; i+1 < len(l[1]); i += 2 {
				v, _ := strconv.ParseUint(l[1][i:i+2], 16, 8)
				c.name = append(c.name, byte(v))
			}
			c.lookup = len(l) > 2 && l[2] == "lookup=ok"
		}
		m.present = append(m.present, c)
	}

	if lines, err = readLines(p("lookup.tsv.gz")); err != nil {
		return nil, err
	}
	for _, l := range lines {
		v, _ := strconv.ParseUint(l[0], 16, 16)
		c := lookupCase{u: uint16(v)}
		for _, f := range l[1:] {
			k, val, _ := strings.Cut(f, "=")
			c.probes = append(c.probes, lookupProbe{k, val == "1"})
		}
		m.lookup = append(m.lookup, c)
	}

	if lines, err = readLines(p("probe.tsv.gz")); err != nil {
		return nil, err
	}
	for _, l := range lines {
		u, _ := strconv.ParseUint(l[0], 16, 16)
		pf, _ := strconv.ParseUint(l[1], 16, 16)
		c := probeCase{u: uint16(u), prefix: uint16(pf)}
		for _, f := range strings.Fields(l[2]) {
			k, _ := strconv.ParseUint(f[:4], 16, 16)
			c.rels = append(c.rels, probeRel{uint16(k), f[4]})
		}
		m.probe = append(m.probe, c)
	}

	if m.facts, err = readLines(p("facts.tsv")); err != nil {
		return nil, err
	}
	return m, nil
}

// readFoldFile reads the compact format written by measureFold.
func readFoldFile(path string) (foldFile, error) {
	var f foldFile
	raw, err := readAllLines(path)
	if err != nil {
		return f, err
	}
	for _, l := range raw {
		switch {
		case strings.HasPrefix(l, "#kct "):
			v, _ := strconv.ParseUint(l[5:], 16, 8)
			f.kct = uint8(v)
		case strings.HasPrefix(l, "#pre "):
			f.pre, err = parseRunes(l[5:])
		case strings.HasPrefix(l, "#post "):
			f.post, err = parseRunes(l[6:])
		case strings.HasPrefix(l, "#") || l == "":
		case strings.HasPrefix(l, "R\t"):
			fl := strings.Split(l, "\t")
			a, _ := strconv.ParseUint(fl[1], 16, 16)
			b, _ := strconv.ParseUint(fl[2], 16, 16)
			for v := a; v <= b; v++ {
				f.entries = append(f.entries, foldEntry{mid: []uint16{uint16(v)}, input: string(rune(v))})
			}
		default:
			fl := strings.Split(l, "\t")
			var e foldEntry
			if e.mid, err = parseUnits(fl[0]); err != nil {
				return f, err
			}
			if e.input, err = parseRunes(fl[1]); err != nil {
				return f, err
			}
			for _, c := range fl[2:] {
				s, err := parseRunes(c)
				if err != nil {
					return f, err
				}
				e.colliders = append(e.colliders, s)
			}
			f.entries = append(f.entries, e)
		}
		if err != nil {
			return f, err
		}
	}
	return f, nil
}

func readOrderFile(path string) (orderFile, error) {
	var f orderFile
	raw, err := readAllLines(path)
	if err != nil {
		return f, err
	}
	for _, l := range raw {
		switch {
		case strings.HasPrefix(l, "#kct "):
			v, _ := strconv.ParseUint(l[5:], 16, 8)
			f.kct = uint8(v)
		case strings.HasPrefix(l, "#") || l == "":
		default:
			fl := strings.Split(l, "\t")
			var e orderEntryM
			if e.stored, err = parseUnits(fl[0]); err != nil {
				return f, err
			}
			if e.input, err = parseRunes(fl[1]); err != nil {
				return f, err
			}
			for _, c := range fl[2:] {
				s, err := parseRunes(c)
				if err != nil {
					return f, err
				}
				e.colliders = append(e.colliders, s)
			}
			f.entries = append(f.entries, e)
		}
	}
	return f, nil
}

// readAllLines returns every line of path, comments included.
func readAllLines(path string) ([]string, error) { return readLinesRaw(path) }
