package main

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// ucdChar is the part of a UnicodeData.txt entry the generator needs.
type ucdChar struct {
	CCC     uint8
	Decomp  []rune // canonical decomposition mapping (one level), nil if none or compatibility
	Compat  bool   // decomposition field is a <tagged> compatibility mapping
	Upper   rune   // simple uppercase, 0 if none
	Lower   rune   // simple lowercase, 0 if none
	Title   rune   // simple titlecase, 0 if none
	Name    string
	Defined bool
}

type ucd struct {
	chars map[rune]*ucdChar
}

func (u *ucd) get(r rune) *ucdChar {
	if c := u.chars[r]; c != nil {
		return c
	}
	return &ucdChar{}
}

// readUCD parses UnicodeData.txt (optionally gzipped). Ranges given by
// "<..., First>"/"<..., Last>" pairs are expanded (they carry no
// decompositions, cases or combining classes, but are marked defined).
func readUCD(path string) (*ucd, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	}
	u := &ucd{chars: map[rune]*ucdChar{}}
	sc := bufio.NewScanner(r)
	var first rune = -1
	line := 0
	for sc.Scan() {
		line++
		fl := strings.Split(sc.Text(), ";")
		if len(fl) < 15 {
			continue
		}
		cp, err := strconv.ParseUint(fl[0], 16, 32)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %v", path, line, err)
		}
		c := &ucdChar{Name: fl[1], Defined: true}
		ccc, _ := strconv.Atoi(fl[3])
		c.CCC = uint8(ccc)
		if d := fl[5]; d != "" {
			if strings.HasPrefix(d, "<") {
				c.Compat = true
			} else {
				for _, h := range strings.Fields(d) {
					v, err := strconv.ParseUint(h, 16, 32)
					if err != nil {
						return nil, fmt.Errorf("%s:%d: %v", path, line, err)
					}
					c.Decomp = append(c.Decomp, rune(v))
				}
			}
		}
		pc := func(s string) rune {
			if s == "" {
				return 0
			}
			v, _ := strconv.ParseUint(s, 16, 32)
			return rune(v)
		}
		c.Upper, c.Lower, c.Title = pc(fl[12]), pc(fl[13]), pc(fl[14])
		switch {
		case strings.HasSuffix(fl[1], ", First>"):
			first = rune(cp)
		case strings.HasSuffix(fl[1], ", Last>"):
			for x := first; x <= rune(cp); x++ {
				u.chars[x] = &ucdChar{Name: fl[1], Defined: true, CCC: c.CCC}
			}
			first = -1
		default:
			u.chars[rune(cp)] = c
		}
	}
	return u, sc.Err()
}
