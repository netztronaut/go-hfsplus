package hfsplus

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func TestInstalledAndErased(t *testing.T) {
	for _, l := range []string{"apm", "gpt"} {
		t.Run(l, func(t *testing.T) {
			inst := openImage(t, "jhfsplus-"+l, Options{})
			b, ok, err := inst.Blessed()
			if err != nil || !ok {
				t.Fatalf("installed: Blessed = %+v, %v, %v", b, ok, err)
			}
			if b.SystemFolderPath != "System/Library/CoreServices" || b.OSXFolderPath != "System/Library/CoreServices" {
				t.Errorf("blessed folder paths %q, %q", b.SystemFolderPath, b.OSXFolderPath)
			}
			if b.BootFilePath != "System/Library/CoreServices/BootX" {
				t.Errorf("blessed file path %q", b.BootFilePath)
			}
			w := blessInfo(t, "jhfsplus-"+l)
			if got := [6]uint32{b.SystemFolder, b.BootFile, b.OpenFolder, b.OS9Folder, 0, b.OSXFolder}; got != w {
				t.Errorf("finderinfo %v, bless --info says %v", got, w)
			}
			plist, err := fs.ReadFile(inst, "System/Library/CoreServices/SystemVersion.plist")
			if err != nil || !bytes.Contains(plist, []byte("<string>10.4.11</string>")) {
				t.Errorf("SystemVersion.plist: %v\n%s", err, plist)
			}

			erased := openImage(t, "erased-jhfsplus-"+l, Options{})
			if b, ok, err := erased.Blessed(); err != nil || ok {
				t.Errorf("erased: Blessed = %+v, %v, %v", b, ok, err)
			}
			if _, err := fs.Stat(erased, "System/Library/CoreServices/SystemVersion.plist"); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("erased: SystemVersion.plist: %v", err)
			}
			if name, err := erased.Name(); err != nil || name != "erased-jhfsplus-"+l {
				t.Errorf("erased: Name = %q, %v", name, err)
			}
		})
	}
}

func TestHeader(t *testing.T) {
	v := openImage(t, "jhfsx-apm", Options{})
	h := v.Header()
	if !h.IsHFSX() || h.Version != 5 || !v.CaseSensitive() {
		t.Errorf("HFSX: signature %#x version %d case-sensitive %v", h.Signature, h.Version, v.CaseSensitive())
	}
	raw := h.Raw()
	if string(raw[:2]) != "HX" {
		t.Errorf("raw header starts %q", raw[:2])
	}
	if h.BlockSize != 4096 || h.TotalBlocks == 0 || h.FileCount == 0 {
		t.Errorf("header %+v", h)
	}
	j := v.Journal()
	if !j.Journaled || !j.CleanlyUnmounted || j.Pending || j.Replayed || !j.HeaderValid || !j.InFilesystem {
		t.Errorf("journal of a cleanly detached image: %+v", j)
	}
	v = openImage(t, "hfsplus-apm", Options{})
	if h := v.Header(); h.IsHFSX() || v.CaseSensitive() {
		t.Errorf("HFS+ reported as HFSX")
	}
	if j := v.Journal(); j.Journaled || !j.CleanlyUnmounted {
		t.Errorf("journal of a non-journaled image: %+v", j)
	}
}

func TestCaseSensitivity(t *testing.T) {
	ci := openImage(t, "jhfsplus-apm", Options{})
	if b, err := ci.ReadFile("NAMES/readme"); err != nil || string(b) != "README" {
		t.Errorf("case-insensitive lookup: %q, %v", b, err)
	}
	cs := openImage(t, "jhfsx-apm", Options{})
	if b, err := cs.ReadFile("names/readme"); err != nil || string(b) != "lower case\n" {
		t.Errorf("case-sensitive lookup: %q, %v", b, err)
	}
	if _, err := cs.ReadFile("NAMES/readme"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("case-sensitive volume found NAMES: %v", err)
	}
}

func TestNames(t *testing.T) {
	v := openImage(t, "jhfsplus-gpt", Options{})
	for name, want := range map[string]string{
		"names/précomposé.txt":       "précomposé.txt",  // precomposed lookup of a decomposed name
		"names/précomposé.txt":     "précomposé.txt",  // decomposed lookup
		"names/décomposé.txt":        "décomposé.txt", // created decomposed
		"names/a:b.txt":              "a:b.txt",
		"names/한국어.txt":              "한국어.txt",
		"names/emoji-\U0001F600.txt": "emoji-\U0001F600.txt",
	} {
		b, err := v.ReadFile(name)
		if err != nil {
			t.Errorf("%q: %v", name, err)
			continue
		}
		if !strings.Contains(string(b), strings.TrimSuffix(want, ".txt")[:1]) {
			t.Errorf("%q: contents %q", name, b)
		}
	}
	es, err := v.ReadDir("names")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range es {
		got = append(got, e.Name())
	}
	for _, want := range []string{"a:b.txt", "précomposé.txt", "한국어.txt"} {
		found := false
		for _, g := range got {
			found = found || g == want
		}
		if !found {
			t.Errorf("ReadDir(names) lacks %q: %q", want, got)
		}
	}
}

func TestSymlinks(t *testing.T) {
	v := openImage(t, "hfsplus-apm", Options{})
	want, err := v.ReadFile("a/b/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"links/rel", "links/abs", "links/chain", "links/dir/b/file.txt", "links/above-root"} {
		if b, err := v.ReadFile(p); err != nil || !bytes.Equal(b, want) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if b, err := v.ReadFile("etc/hosts"); err != nil || !strings.Contains(string(b), "localhost") {
		t.Errorf("etc/hosts through /etc -> private/etc: %q, %v", b, err)
	}
	if _, err := v.ReadFile("links/loop1"); !errors.Is(err, ErrLinkLoop) {
		t.Errorf("loop: %v", err)
	}
	if _, err := v.Stat("links/broken"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("broken: %v", err)
	}
	if fi, err := v.Lstat("links/loop1"); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("Lstat(loop1) = %v, %v", fi, err)
	}
	if target, err := fs.ReadLink(v, "links/abs"); err != nil || target != "/a/b/file.txt" {
		t.Errorf("ReadLink = %q, %v", target, err)
	}
	if _, err := fs.ReadLink(v, "a/b/file.txt"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("ReadLink of a file: %v", err)
	}
	if _, err := v.ReadFile("a/b/file.txt/x"); !errors.Is(err, ErrNotDir) {
		t.Errorf("through a file: %v", err)
	}
}

func TestHardLinks(t *testing.T) {
	v := openImage(t, "jhfsplus-apm", Options{})
	a, err := v.Stat("hard/orig.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, err := v.Stat("hard/sub/link2.txt")
	if err != nil {
		t.Fatal(err)
	}
	ra, rb := a.Sys().(*Record), b.Sys().(*Record)
	if ra.CNID != rb.CNID || ra.Link == nil || rb.Link == nil || ra.BSD.Special != 3 {
		t.Errorf("hard links: %+v %+v", ra, rb)
	}
	if b.Name() != "link2.txt" {
		t.Errorf("name %q", b.Name())
	}
	fi, err := v.Stat("dirlink-dst/linked")
	if err != nil || !fi.IsDir() {
		t.Fatalf("directory hard link: %v, %v", fi, err)
	}
	if b, err := v.ReadFile("dirlink-dst/linked/inner/f.txt"); err != nil || string(b) != "in a hard-linked directory\n" {
		t.Errorf("through a directory hard link: %q, %v", b, err)
	}
	// The private directories are hidden, unless asked for.
	if _, err := v.Stat("␀␀␀␀HFS+ Private Data"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("private directory visible: %v", err)
	}
	p := openImage(t, "jhfsplus-apm", Options{ShowPrivate: true})
	es, err := p.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	for _, want := range []string{"␀␀␀␀HFS+ Private Data", ".HFS+ Private Directory Data\r", ".journal", ".journal_info_block"} {
		found := false
		for _, n := range names {
			found = found || n == want
		}
		if !found {
			t.Errorf("ShowPrivate: %q missing from %q", want, names)
		}
	}
}

func TestFragmented(t *testing.T) {
	// The journaled images have it fragmented; on the others HFS+ found a contiguous run.
	v := openImage(t, "jhfsplus-gpt", Options{})
	r, err := v.Record("fragmented.bin")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range r.DataFork.Extents {
		if e.BlockCount > 0 {
			n++
		}
	}
	var covered uint32
	for _, e := range r.DataFork.Extents {
		covered += e.BlockCount
	}
	if n != 8 || covered >= r.DataFork.TotalBlocks {
		t.Fatalf("fragmented.bin does not use the extents overflow file: %d extents cover %d of %d blocks", n, covered, r.DataFork.TotalBlocks)
	}
	b, err := v.ReadFile("fragmented.bin")
	if err != nil || int64(len(b)) != int64(r.DataFork.LogicalSize) {
		t.Fatalf("ReadFile: %d bytes, %v", len(b), err)
	}
	// Read it again through the file, in odd-sized pieces and backwards.
	f, err := v.Open("fragmented.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ra := f.(io.ReaderAt)
	for off := int64(len(b)) - 1000; off > 0; off -= 3001 {
		p := make([]byte, 1000)
		if _, err := ra.ReadAt(p, off); err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if !bytes.Equal(p, b[off:off+1000]) {
			t.Fatalf("ReadAt(%d) differs", off)
		}
	}
}

func TestCompressed(t *testing.T) {
	v := openImage(t, "jhfsplus-apm", Options{})
	types := map[uint32]bool{}
	var files []string
	fs.WalkDir(v, "compressed", func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			files = append(files, p)
		}
		return err
	})
	for _, p := range files {
		r, err := v.Record(p)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Compressed() {
			continue
		}
		as, err := v.Attributes(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			if a.Name == XattrDecmpfs {
				raw, err := v.attr(context.Background(), r.CNID, XattrDecmpfs)
				if err != nil {
					t.Fatal(err)
				}
				h, err := decodeDecmpfs(raw.inline)
				if err != nil {
					t.Fatal(err)
				}
				types[h.Type] = true
			}
		}
	}
	for _, want := range []uint32{1, 3, 4, 7, 8, 11, 12} {
		if !types[want] {
			t.Errorf("no compressed file of type %d in the image: %v", want, types)
		}
	}
	// A type the reader does not know is an error, never the compressed bytes.
	u := openImage(t, "jhfsplus-apm", Options{Decompressors: map[uint32]Decompressor{}})
	for tp := range types {
		delete(u.decompressors, tp)
	}
	for _, p := range files {
		r, _ := u.Record(p)
		if !r.Compressed() || strings.Contains(p, "type1-") {
			continue
		}
		if _, err := u.ReadFile(p); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s without its decompressor: %v", p, err)
		}
	}
}

func TestXattrs(t *testing.T) {
	v := openImage(t, "jhfsplus-apm", Options{})
	b, err := v.GetXattr("xattr/file.txt", "user.small")
	if err != nil || string(b) != "hello" {
		t.Errorf("user.small = %q, %v", b, err)
	}
	b, err = v.GetXattr("xattr/file.txt", "user.huge")
	if err != nil || len(b) != 100000 {
		t.Errorf("user.huge: %d bytes, %v", len(b), err)
	}
	as, err := v.Attributes("xattr/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	inline, forked := false, false
	for _, a := range as {
		inline = inline || a.Inline
		forked = forked || !a.Inline
	}
	if !inline || !forked {
		t.Errorf("attributes %+v: want both inline and fork records", as)
	}
	if _, err := v.GetXattr("xattr/file.txt", "user.none"); !errors.Is(err, ErrNoXattr) {
		t.Errorf("missing attribute: %v", err)
	}
	fi, err := v.GetXattr("xattr/typed.txt", XattrFinderInfo)
	if err != nil || string(fi[:8]) != "TEXTttxt" {
		t.Errorf("FinderInfo = %q, %v", fi, err)
	}
	r, _ := v.Record("xattr/typed.txt")
	if r.Type() != fourCC("TEXT") || r.Creator() != fourCC("ttxt") {
		t.Errorf("type/creator %#x/%#x", r.Type(), r.Creator())
	}
	rf, err := v.OpenResourceFork("rsrc/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	rb, err := io.ReadAll(rf)
	if err != nil || len(rb) != 5000 {
		t.Errorf("resource fork: %d bytes, %v", len(rb), err)
	}
	xb, err := v.GetXattr("rsrc/file.txt", XattrResourceFork)
	if err != nil || !bytes.Equal(xb, rb) {
		t.Errorf("resource fork attribute differs: %v", err)
	}
}

func TestFSTest(t *testing.T) {
	v := openImage(t, "jhfsplus-apm", Options{})
	for _, dir := range []string{"a", "names", "compressed", "xattr", "hard", "dirlink-src", "dirlink-dst", "System", "private", "modes", "rsrc"} {
		t.Run(dir, func(t *testing.T) {
			sub, err := fs.Sub(v, dir)
			if err != nil {
				t.Fatal(err)
			}
			var expected []string
			fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
				if err == nil && p != "." {
					expected = append(expected, p)
				}
				return nil
			})
			if err := fstest.TestFS(sub, expected...); err != nil {
				t.Error(err)
			}
		})
	}
	if err := fstest.TestFS(v, "System/Library/CoreServices/SystemVersion.plist", "a/b/c/d/e/deep.txt"); err != nil {
		// The whole volume includes the symbolic link loop, which Open cannot follow.
		for _, line := range strings.Split(err.Error(), "\n") {
			if !strings.Contains(line, "links/loop") && !strings.Contains(line, "links/broken") && line != "" && line != "TestFS found errors:" {
				t.Error(line)
			}
		}
	}
}

func TestDirtyJournal(t *testing.T) {
	v := openImage(t, "dirty-jhfsplus-apm", Options{})
	j := v.Journal()
	t.Logf("journal: %+v", j)
	if !j.Journaled || j.CleanlyUnmounted || !j.Pending || !j.Replayed || j.Blocks == 0 {
		t.Fatalf("dirty journal reported as %+v", j)
	}
	// The replay has to matter: blocks the overlay supplies differ from their home locations.
	differ := 0
	for _, blk := range v.dev.overlayBlocks() {
		a, b := make([]byte, v.dev.unit), make([]byte, v.dev.unit)
		v.dev.ReadAt(a, blk*v.dev.unit)
		v.dev.r.ReadAt(b, blk*v.dev.unit)
		if !bytes.Equal(a, b) {
			differ++
		}
	}
	if differ == 0 {
		t.Errorf("none of the %d replayed blocks differs from the disk", len(v.dev.overlay))
	}
	if got, want := listing(t, v), golden(t, "dirty-jhfsplus-apm"); got != want {
		t.Errorf("replayed listing differs from macOS's after its replay (- macOS, + reader):\n%s", diffLines(want, got))
	}

	disk := loadImage(t, "dirty-jhfsplus-apm")
	off, size := partitionOf(t, disk)
	part := bytes.NewReader(disk[off : off+size])
	if _, err := Open(part, size, Options{Journal: Refuse}); !errors.Is(err, ErrJournalNotReplayed) {
		t.Errorf("Refuse: %v", err)
	}
	od, err := Open(part, size, Options{Journal: OnDisk})
	if err != nil {
		t.Fatalf("OnDisk: %v", err)
	}
	if j := od.Journal(); !j.Pending || j.Replayed {
		t.Errorf("OnDisk journal %+v", j)
	}
	if _, err := od.Stat("after/d39/f"); err == nil {
		t.Errorf("the on-disk state has the file only the journal holds")
	}
	// The image is read, never written: replay must not have changed a byte.
	fresh := loadImageUncached(t, "dirty-jhfsplus-apm")
	if !bytes.Equal(fresh, disk) {
		t.Errorf("the image changed")
	}
}

func loadImageUncached(t testing.TB, name string) []byte {
	imagesMu.Lock()
	b := images[name]
	delete(images, name)
	imagesMu.Unlock()
	fresh := loadImage(t, name)
	imagesMu.Lock()
	images[name] = b
	imagesMu.Unlock()
	return fresh
}

// TestHFSXCaseFolding makes an HFSX volume whose catalog compares names case-folding (key compare
// type 0xCF) out of an HFS+ one, whose catalog is in that order already. Synthesised: hdiutil
// makes HFSX volumes case-sensitive only.
func TestHFSXCaseFolding(t *testing.T) {
	disk := loadImage(t, "hfsplus-apm")
	off, size := partitionOf(t, disk)
	part := bytes.Clone(disk[off : off+size])
	h := decodeVolumeHeader(part[headerOffset:])
	copy(part[headerOffset:], "HX")
	part[headerOffset+3] = 5
	cat := int64(h.CatalogFile.Extents[0].StartBlock) * int64(h.BlockSize)
	part[cat+nodeDescriptorSize+37] = keyCompareFolding
	v, err := Open(bytes.NewReader(part), size, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if v.CaseSensitive() || !v.Header().IsHFSX() {
		t.Fatalf("case-folding HFSX reported case-sensitive")
	}
	if b, err := v.ReadFile("NAMES/readme"); err != nil || string(b) != "README" {
		t.Errorf("case-folding lookup: %q, %v", b, err)
	}
	part[cat+nodeDescriptorSize+37] = 0x42
	if _, err := Open(bytes.NewReader(part), size, Options{}); !errors.Is(err, ErrCorrupt) {
		t.Errorf("unknown key compare type: %v", err)
	}
}

// TestWrapper embeds an HFS+ volume in a synthesised HFS wrapper, as the 10.0-10.2 era tools
// initialised disks: a Master Directory Block whose embedded extent holds the HFS+ volume.
func TestWrapper(t *testing.T) {
	disk := loadImage(t, "hfsplus-apm")
	off, size := partitionOf(t, disk)
	const alBlSt, alBlkSiz, embedStart = 16, 4096, 2
	embOff := int64(alBlSt*512 + embedStart*alBlkSiz)
	part := make([]byte, embOff+size+alBlkSiz)
	copy(part[embOff:], disk[off:off+size])
	mdb := part[headerOffset:]
	copy(mdb, "BD")
	be := func(b []byte, v uint32, n int) {
		for i := range n {
			b[i] = byte(v >> (8 * (n - 1 - i)))
		}
	}
	be(mdb[20:], alBlkSiz, 4)
	be(mdb[28:], alBlSt, 2)
	copy(mdb[124:], "H+")
	be(mdb[126:], embedStart, 2)
	be(mdb[128:], uint32(size/alBlkSiz), 2)
	v, err := Open(bytes.NewReader(part), int64(len(part)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	w := v.Wrapper()
	if w == nil || w.Offset != embOff || w.Length != size {
		t.Fatalf("wrapper %+v", w)
	}
	if got, want := listing(t, v), golden(t, "hfsplus-apm"); got != want {
		t.Errorf("listing through the wrapper differs:\n%s", diffLines(want, got))
	}
	copy(mdb[124:], "\x00\x00")
	if _, err := Open(bytes.NewReader(part), int64(len(part)), Options{}); !errors.Is(err, ErrNotHFSPlus) {
		t.Errorf("HFS standard: %v", err)
	}
}

func TestNotHFSPlus(t *testing.T) {
	for _, b := range [][]byte{nil, make([]byte, 4096), bytes.Repeat([]byte{0xFF}, 8192)} {
		if _, err := Open(bytes.NewReader(b), int64(len(b)), Options{}); !errors.Is(err, ErrNotHFSPlus) {
			t.Errorf("%d bytes: %v", len(b), err)
		}
	}
	// A whole disk is not a volume.
	disk := loadImage(t, "hfsplus-apm")
	if _, err := Open(bytes.NewReader(disk), int64(len(disk)), Options{}); !errors.Is(err, ErrNotHFSPlus) {
		t.Errorf("whole disk: %v", err)
	}
}

func TestContextCancel(t *testing.T) {
	v := openImage(t, "jhfsplus-apm", Options{CacheSize: -1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var seen error
	fs.WalkDir(v.WithContext(ctx), ".", func(_ string, _ fs.DirEntry, err error) error {
		seen = cmp.Or(seen, err)
		return err
	})
	if !errors.Is(seen, context.Canceled) {
		t.Errorf("walk with a canceled context: %v", seen)
	}
}

func TestTruncated(t *testing.T) {
	// A device that ends early, as a torn or short read leaves it, gives errors, never a panic.
	disk := loadImage(t, "jhfsplus-apm")
	off, size := partitionOf(t, disk)
	part := disk[off : off+size]
	for _, n := range []int64{1024, 1536, 4096, 8192, 65536, size / 4, size / 2, size - 4096} {
		v, err := Open(bytes.NewReader(part[:n]), size, Options{})
		if err != nil {
			continue
		}
		fs.WalkDir(v, ".", func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				v.ReadFile(p)
			}
			return nil
		})
	}
}
