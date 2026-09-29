package hfsplus

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"netztronaut.de/go-hfsplus/apm"
)

// The images in ../testdata/images are made by ../testdata/mkfixtures.sh on macOS; each has a
// golden listing macOS itself produced from the attached image.

var (
	imagesMu sync.Mutex
	images   = map[string][]byte{}
)

func loadImage(t testing.TB, name string) []byte {
	t.Helper()
	imagesMu.Lock()
	defer imagesMu.Unlock()
	if b, ok := images[name]; ok {
		return b
	}
	f, err := os.Open(filepath.Join("..", "testdata", "images", name+".img.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	images[name] = b
	return b
}

var hfsPlusGUID = [16]byte{0x00, 0x53, 0x46, 0x48, 0x00, 0x00, 0xAA, 0x11, 0xAA, 0x11, 0x00, 0x30, 0x65, 0x43, 0xEC, 0xAC}

// partitionOf finds the HFS+ partition of a disk image with an APM or a GPT, as rawdisk and apm
// would for the orchestrator.
func partitionOf(t testing.TB, disk []byte) (off, size int64) {
	t.Helper()
	if string(disk[512:520]) == "EFI PART" {
		h := disk[512:]
		lba := int64(binary.LittleEndian.Uint64(h[72:]))
		n := int(binary.LittleEndian.Uint32(h[80:]))
		es := int64(binary.LittleEndian.Uint32(h[84:]))
		for i := range n {
			e := disk[lba*512+int64(i)*es:]
			if [16]byte(e[:16]) == hfsPlusGUID {
				first := int64(binary.LittleEndian.Uint64(e[32:]))
				last := int64(binary.LittleEndian.Uint64(e[40:]))
				return first * 512, (last - first + 1) * 512
			}
		}
		t.Fatal("no HFS+ partition in the GPT")
	}
	m, err := apm.Read(bytes.NewReader(disk), int64(len(disk)))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range m.Partitions {
		if p.Type == apm.TypeHFS || p.Type == apm.TypeHFSX {
			return p.Start, p.Length
		}
	}
	t.Fatal("no Apple_HFS partition in the APM")
	return 0, 0
}

func openImage(t testing.TB, name string, opts Options) *Volume {
	t.Helper()
	disk := loadImage(t, name)
	off, size := partitionOf(t, disk)
	v, err := Open(bytes.NewReader(disk[off:off+size]), size, opts)
	if err != nil {
		t.Fatalf("%s: Open: %v", name, err)
	}
	return v
}

// listing prints the volume as testdata/listing.py prints a mounted one.
func listing(t testing.TB, v *Volume) string {
	t.Helper()
	var lines []string
	err := fs.WalkDir(v, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := v.Lstat(p)
		if err != nil {
			return err
		}
		r := fi.Sys().(*Record)
		m := fi.Mode()
		kind, content, size, nlink := "f", "-", fi.Size(), uint32(1)
		switch {
		case m.IsDir():
			kind, size, nlink = "d", 0, 0
		case m&fs.ModeSymlink != 0:
			kind = "l"
			if content, err = v.ReadLink(p); err != nil {
				return err
			}
		case m&fs.ModeNamedPipe != 0:
			kind = "p"
		default:
			b, err := v.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			content = hex.EncodeToString(sum[:])
		}
		if r.Link != nil && !r.IsFolder {
			nlink = r.BSD.Special
		}
		perm := uint32(m.Perm())
		if m&fs.ModeSetuid != 0 {
			perm |= 0o4000
		}
		if m&fs.ModeSetgid != 0 {
			perm |= 0o2000
		}
		if m&fs.ModeSticky != 0 {
			perm |= 0o1000
		}
		names, err := v.ListXattr(p)
		if err != nil {
			return err
		}
		var xs []string
		for _, x := range names {
			b, err := v.GetXattr(p, x)
			if err != nil {
				return fmt.Errorf("%s: %s: %w", p, x, err)
			}
			sum := sha256.Sum256(b)
			xs = append(xs, x+"="+hex.EncodeToString(sum[:])[:16])
		}
		x := strings.Join(xs, ",")
		if x == "" {
			x = "-"
		}
		lines = append(lines, strings.Join([]string{
			p, kind, fmt.Sprintf("%o", perm), fmt.Sprint(r.BSD.OwnerID), fmt.Sprint(r.BSD.GroupID),
			fmt.Sprint(nlink), fmt.Sprint(r.CNID), fmt.Sprint(size), fmt.Sprint(fi.ModTime().Unix()), content, x,
		}, "\t"))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n") + "\n"
}

func golden(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "images", name+".golden"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// diffLines reports the lines only in want and only in got.
func diffLines(want, got string) string {
	w := strings.Split(strings.TrimSuffix(want, "\n"), "\n")
	g := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	var b strings.Builder
	for _, l := range w {
		if !slices.Contains(g, l) {
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}
	for _, l := range g {
		if !slices.Contains(w, l) {
			fmt.Fprintf(&b, "+ %s\n", l)
		}
	}
	return b.String()
}

var fixtureNames = []string{
	"hfsplus-apm", "hfsplus-gpt", "jhfsplus-apm", "jhfsplus-gpt",
	"hfsx-apm", "hfsx-gpt", "jhfsx-apm", "jhfsx-gpt",
	"erased-jhfsplus-apm", "erased-jhfsplus-gpt",
}

func TestGoldenListings(t *testing.T) {
	for _, name := range fixtureNames {
		t.Run(name, func(t *testing.T) {
			v := openImage(t, name, Options{})
			got := listing(t, v)
			if want := golden(t, name); got != want {
				t.Errorf("listing differs from macOS's (- macOS, + reader):\n%s", diffLines(want, got))
			}
		})
	}
}

// blessInfo reads the finderinfo words bless --info printed for an image.
func blessInfo(t testing.TB, name string) [6]uint32 {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "testdata", "images", name+".bless"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var w [6]uint32
	s := bufio.NewScanner(f)
	for s.Scan() {
		var i int
		var x uint32
		if n, _ := fmt.Sscanf(s.Text(), "finderinfo[%d]: %d", &i, &x); n == 2 && i < 6 {
			w[i] = x
		}
	}
	return w
}
