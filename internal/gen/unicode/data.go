package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Text formats of the measurement files in testdata/. Units and runes are
// upper-case hex separated by single spaces; "-" is the empty sequence.

func fmtUnits(u []uint16) string {
	if len(u) == 0 {
		return "-"
	}
	var b strings.Builder
	for i, x := range u {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%04X", x)
	}
	return b.String()
}

func fmtRunes(s string) string {
	if s == "" {
		return "-"
	}
	var b strings.Builder
	for i, r := range []rune(s) {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%04X", r)
	}
	return b.String()
}

func parseHexList(s string) ([]uint32, error) {
	if s == "-" {
		return nil, nil
	}
	var out []uint32
	for _, f := range strings.Fields(s) {
		v, err := strconv.ParseUint(f, 16, 32)
		if err != nil {
			return nil, err
		}
		out = append(out, uint32(v))
	}
	return out, nil
}

func parseUnits(s string) ([]uint16, error) {
	l, err := parseHexList(s)
	if err != nil {
		return nil, err
	}
	u := make([]uint16, len(l))
	for i, v := range l {
		if v > 0xFFFF {
			return nil, fmt.Errorf("unit %X out of range", v)
		}
		u[i] = uint16(v)
	}
	return u, nil
}

func parseRunes(s string) (string, error) {
	l, err := parseHexList(s)
	if err != nil {
		return "", err
	}
	r := make([]rune, len(l))
	for i, v := range l {
		r[i] = rune(v)
	}
	return string(r), nil
}

// writeLines writes lines to path, gzip-compressed when path ends in .gz.
// The gzip header carries no name or time, so output is reproducible.
func writeLines(path string, lines []string) error {
	var buf bytes.Buffer
	var w io.Writer = &buf
	var zw *gzip.Writer
	if strings.HasSuffix(path, ".gz") {
		zw, _ = gzip.NewWriterLevel(&buf, gzip.BestCompression)
		w = zw
	}
	for _, l := range lines {
		io.WriteString(w, l)
		io.WriteString(w, "\n")
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			return err
		}
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// readLinesRaw returns the lines of path, gunzipped when path ends in .gz.
func readLinesRaw(path string) ([]string, error) {
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
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out, sc.Err()
}

// readLines reads the non-empty, non-comment lines of path, split on tabs.
func readLines(path string) ([][]string, error) {
	raw, err := readLinesRaw(path)
	if err != nil {
		return nil, err
	}
	var out [][]string
	for _, l := range raw {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, strings.Split(l, "\t"))
	}
	return out, nil
}
