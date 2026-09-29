// Command unicode generates hfsplus/unicode_tables.go: Apple's HFS+
// decomposition, combining-class and case-folding tables.
//
// The tables are derived from the Unicode 3.2 character database
// (testdata/UnicodeData-3.2.0.txt.gz) and then checked against, and where they
// disagree corrected by, measurements of the macOS kernel's HFS+
// implementation kept in testdata/*.tsv*. With -measure (macOS only) the
// measurements are taken afresh: names are created through POSIX on hdiutil
// images and what the kernel stored is read back from the raw catalog B-tree.
//
// Usage, from the module root:
//
//	go run ./internal/gen/unicode            # regenerate from the checked-in measurements
//	go run ./internal/gen/unicode -measure   # re-measure the kernel first (macOS, a few minutes)
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	var (
		measure  = flag.Bool("measure", false, "measure the running macOS kernel and rewrite testdata/")
		testdata = flag.String("testdata", "internal/gen/unicode/testdata", "measurement and UCD directory")
		out      = flag.String("o", "hfsplus/unicode_tables.go", "output file")
		work     = flag.String("work", "", "scratch directory for disk images (default: a temporary directory)")
	)
	flag.Parse()
	u, err := readUCD(filepath.Join(*testdata, "UnicodeData-3.2.0.txt.gz"))
	if err != nil {
		fatal(err)
	}
	if *measure {
		w := *work
		if w == "" {
			w, err = os.MkdirTemp("", "hfsunicode")
			if err != nil {
				fatal(err)
			}
			defer os.RemoveAll(w)
		}
		if err := runMeasure(w, *testdata, u); err != nil {
			fatal(err)
		}
	}
	if err := generate(u, *testdata, *out); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gen/unicode:", err)
	os.Exit(1)
}
