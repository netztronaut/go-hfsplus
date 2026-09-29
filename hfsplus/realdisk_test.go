package hfsplus

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"netztronaut.de/go-hfsplus/apm"
)

// Real disks are too large to check in. These tests read them when an environment variable points
// at a raw image, and skip otherwise.

// openRawDisk opens a raw disk image and the HFS+ volume of its APM or GPT.
func openRawDisk(t *testing.T, path string) (*Volume, string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	// A device's size and alignment are the caller's: Stat says 0 for one, and a raw device reads
	// only whole sectors.
	total, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatal(err)
	}
	disk := &alignedReaderAt{r: f, align: 4096}
	var off, size int64
	scheme := "apm"
	if m, err := apm.Read(disk, total); err == nil {
		for _, p := range m.Partitions {
			t.Logf("APM entry %d: %q %q, %d bytes at %d", p.Index, p.Name, p.Type, p.Length, p.Start)
		}
		p := m.Find(apm.TypeHFS)
		if p == nil {
			p = m.Find(apm.TypeHFSX)
		}
		if p == nil {
			t.Fatal("no Apple_HFS partition")
		}
		off, size = p.Start, p.Length
	} else if errors.Is(err, apm.ErrNoMap) {
		scheme = "gpt"
		off, size = gptHFSPartition(t, disk)
	} else {
		t.Fatal(err)
	}
	v, err := Open(io.NewSectionReader(disk, off, size), size, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return v, scheme
}

// checkInstalled checks what detection relies on: blessed, a blessed folder that resolves, and a
// SystemVersion.plist naming the release.
func checkInstalled(t *testing.T, v *Volume, version string) {
	t.Helper()
	b, ok, err := v.Blessed()
	if err != nil || !ok {
		t.Fatalf("Blessed = %+v, %v, %v", b, ok, err)
	}
	t.Logf("blessed: %+v", b)
	if b.SystemFolderPath == "" {
		t.Errorf("the blessed System Folder %d does not resolve", b.SystemFolder)
	}
	plist, err := fs.ReadFile(v, "System/Library/CoreServices/SystemVersion.plist")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plist, []byte("<string>"+version)) {
		t.Errorf("SystemVersion.plist does not name %s:\n%s", version, plist)
	}
	t.Logf("journal: %+v", v.Journal())
}

// TestPowerPCDisk reads an installed Mac OS X 10.4 PowerPC disk (HFSPLUS_PPC_IMAGE).
func TestPowerPCDisk(t *testing.T) {
	path := os.Getenv("HFSPLUS_PPC_IMAGE")
	if path == "" {
		t.Skip("HFSPLUS_PPC_IMAGE is not set")
	}
	v, scheme := openRawDisk(t, path)
	if scheme != "apm" {
		t.Errorf("a PowerPC disk with a %s", scheme)
	}
	checkInstalled(t, v, "10.4")
}

// TestPowerPCReleases reads installed PowerPC disks of any release (HFSPLUS_PPC_IMAGES, a list of
// raw images; testdata/utm2raw.sh makes them from UTM virtual machines) and everything on them:
// every entry, the whole of every file and resource fork, every link target and extended
// attribute.
func TestPowerPCReleases(t *testing.T) {
	env := os.Getenv("HFSPLUS_PPC_IMAGES")
	if env == "" {
		t.Skip("HFSPLUS_PPC_IMAGES is not set")
	}
	for _, image := range filepath.SplitList(env) {
		t.Run(filepath.Base(image), func(t *testing.T) {
			v, scheme := openRawDisk(t, image)
			if scheme != "apm" {
				t.Errorf("a PowerPC disk with a %s", scheme)
			}
			checkInstalled(t, v, "10.")
			if w := v.Wrapper(); w != nil {
				t.Logf("HFS wrapper: %d bytes at %d", w.Length, w.Offset)
			}
			walkAll(t, v)
		})
	}
}

// walkAll reads everything on v and checks the entries it reaches do not exceed the volume
// header's counts, which also count what is hidden.
func walkAll(t *testing.T, v *Volume) {
	t.Helper()
	var dirs, files, links, other, rsrc, xattrs int64
	var size int64
	err := fs.WalkDir(v, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		fi, err := v.Lstat(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			return nil
		}
		names, err := v.ListXattr(p)
		if err != nil {
			t.Errorf("%s: listxattr: %v", p, err)
		}
		for _, x := range names {
			xattrs++
			_, err := v.GetXattr(p, x)
			if err != nil && !(x == XattrResourceFork && errors.Is(err, ErrUnsupported)) {
				t.Errorf("%s: getxattr %s: %v", p, x, err)
			}
		}
		switch r := fi.Sys().(*Record); {
		case fi.IsDir():
			dirs++
		case fi.Mode()&fs.ModeSymlink != 0:
			links++
			if _, err := v.ReadLink(p); err != nil {
				t.Errorf("%s: %v", p, err)
			}
			if _, err := v.Stat(p); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, ErrLinkLoop) {
				t.Errorf("%s: stat through the link: %v", p, err)
			}
		case fi.Mode().IsRegular():
			files++
			n, err := readAll(v.Open(p))
			if err != nil || n != fi.Size() {
				t.Errorf("%s: read %d of %d bytes: %v", p, n, fi.Size(), err)
			}
			size += n
			if r.ResourceFork.LogicalSize > 0 {
				rsrc++
				n, err := readAll(v.OpenResourceFork(p))
				if err != nil || uint64(n) != r.ResourceFork.LogicalSize {
					t.Errorf("%s: read %d of %d bytes of the resource fork: %v", p, n, r.ResourceFork.LogicalSize, err)
				}
			}
		default:
			other++
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
	h := v.Header()
	if uint64(files+links+other) > uint64(h.FileCount) || uint64(dirs-1) > uint64(h.FolderCount) {
		t.Errorf("walked %d files and %d folders, the header counts %d and %d", files+links+other, dirs-1, h.FileCount, h.FolderCount)
	}
	t.Logf("%d folders, %d files (%d MiB, %d resource forks), %d symlinks, %d other, %d xattrs; the header counts %d files, %d folders",
		dirs, files, size>>20, rsrc, links, other, xattrs, h.FileCount, h.FolderCount)
}

func readAll[F fs.File](f F, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(io.Discard, f)
}

// TestIntelDisk reads an Intel GPT disk with a JHFS+ install (HFSPLUS_INTEL_IMAGE) and every
// compressed file in /System/Library/CoreServices and /usr/bin.
func TestIntelDisk(t *testing.T) {
	path := os.Getenv("HFSPLUS_INTEL_IMAGE")
	if path == "" {
		t.Skip("HFSPLUS_INTEL_IMAGE is not set")
	}
	v, _ := openRawDisk(t, path)
	checkInstalled(t, v, "10.")
	counts := readCompressed(t, v, "System/Library/CoreServices", "usr/bin")
	t.Logf("compressed files by decmpfs type: %v", counts)
}

// readCompressed reads every compressed file under dirs and counts them by type.
func readCompressed(t *testing.T, v *Volume, dirs ...string) map[uint32]int {
	counts := map[uint32]int{}
	for _, dir := range dirs {
		fs.WalkDir(v, dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			r, err := v.Record(p)
			if err != nil || !r.Compressed() {
				return nil
			}
			a, err := v.attr(v.ctx, r.CNID, XattrDecmpfs)
			if err != nil || a == nil {
				t.Errorf("%s: %v", p, err)
				return nil
			}
			raw, _ := v.readAttr(v.ctx, r.CNID, a, maxDecmpfsAttr)
			h, _ := decodeDecmpfs(raw)
			counts[h.Type]++
			fi, err := v.Stat(p)
			if err != nil {
				t.Errorf("%s: %v", p, err)
				return nil
			}
			b, err := v.ReadFile(p)
			if err != nil || int64(len(b)) != fi.Size() {
				t.Errorf("%s (type %d): %d of %d bytes, %v", p, h.Type, len(b), fi.Size(), err)
			}
			return nil
		})
	}
	return counts
}

// TestMountedMedia compares the reader with macOS on real media that is attached: each element of
// HFSPLUS_MEDIA is IMAGE=MOUNTPOINT, a raw image and where macOS has it attached read-only. Names,
// modes, sizes, modification times and link targets must agree everywhere, contents for files up
// to 1 MiB.
func TestMountedMedia(t *testing.T) {
	env := os.Getenv("HFSPLUS_MEDIA")
	if env == "" {
		t.Skip("HFSPLUS_MEDIA is not set")
	}
	for _, pair := range filepath.SplitList(env) {
		image, mount, ok := strings.Cut(pair, "=")
		if !ok {
			t.Fatalf("%q is not IMAGE=MOUNTPOINT", pair)
		}
		t.Run(filepath.Base(image), func(t *testing.T) {
			v, _ := openRawDisk(t, image)
			if b, ok, err := v.Blessed(); err == nil {
				t.Logf("blessed %v: %+v", ok, b)
			}
			host := os.DirFS(mount)
			n, compared := 0, 0
			err := fs.WalkDir(host, ".", func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					if errors.Is(err, fs.ErrPermission) {
						return fs.SkipDir
					}
					return err
				}
				n++
				hi, err := os.Lstat(filepath.Join(mount, p))
				if err != nil {
					return nil
				}
				vi, err := v.Lstat(p)
				if err != nil {
					t.Errorf("%s: %v", p, err)
					return nil
				}
				if hi.Mode() != vi.Mode() {
					t.Errorf("%s: mode %v, macOS %v", p, vi.Mode(), hi.Mode())
					return nil
				}
				if !hi.ModTime().Equal(vi.ModTime()) {
					t.Errorf("%s: modified %v, macOS %v", p, vi.ModTime(), hi.ModTime())
				}
				switch {
				case hi.Mode()&fs.ModeSymlink != 0:
					ht, _ := os.Readlink(filepath.Join(mount, p))
					if vt, err := v.ReadLink(p); err != nil || vt != ht {
						t.Errorf("%s: link %q, %v, macOS %q", p, vt, err, ht)
					}
				case hi.Mode().IsRegular():
					if hi.Size() != vi.Size() {
						t.Errorf("%s: size %d, macOS %d", p, vi.Size(), hi.Size())
					} else if hi.Size() <= 1<<20 {
						hb, err := os.ReadFile(filepath.Join(mount, p))
						if err != nil {
							return nil
						}
						vb, err := v.ReadFile(p)
						if err != nil || sha256.Sum256(vb) != sha256.Sum256(hb) {
							t.Errorf("%s: contents differ: %v", p, err)
						}
						compared++
					}
				}
				return nil
			})
			if err != nil {
				t.Error(err)
			}
			t.Logf("%d entries agree, %d files compared by contents", n, compared)
			counts := readCompressed(t, v, ".")
			t.Logf("compressed files by decmpfs type: %v", counts)
		})
	}
}

// alignedReaderAt reads whole aligned blocks from r and copies out the bytes asked for.
type alignedReaderAt struct {
	r     io.ReaderAt
	align int64
}

func (a *alignedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	start := off / a.align * a.align
	end := (off + int64(len(p)) + a.align - 1) / a.align * a.align
	buf := make([]byte, end-start)
	n, err := a.r.ReadAt(buf, start)
	if int64(n) <= off-start {
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	m := copy(p, buf[off-start:n])
	if m < len(p) {
		if err == nil {
			err = io.EOF
		}
		return m, err
	}
	return m, nil
}
