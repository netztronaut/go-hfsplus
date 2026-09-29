package hfsplus

import (
	"bytes"
	"encoding/binary"
	"io"
	"io/fs"
	"os"
	"runtime"
	"testing"
)

// heapSampler records the largest live heap above a baseline, sampled after a garbage collection
// at every every-th call of sample.
type heapSampler struct {
	base, peak uint64
	n          int
	every      int
}

func newHeapSampler(every int) *heapSampler {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return &heapSampler{base: ms.HeapAlloc, every: every}
}

func (h *heapSampler) sample() {
	if h.n++; h.n%h.every != 0 {
		return
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if ms.HeapAlloc > h.base && ms.HeapAlloc-h.base > h.peak {
		h.peak = ms.HeapAlloc - h.base
	}
}

// walkMetadata stats every entry of the volume and reads every directory, without reading file
// contents: what detection does, at its most.
func walkMetadata(t testing.TB, v *Volume, sample func()) int {
	n := 0
	err := fs.WalkDir(v, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if _, err := d.Info(); err != nil {
			return err
		}
		n++
		sample()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// memoryCap is the stated bound on the heap a metadata walk holds beyond the node cache: the
// B-tree path being searched, the leaf-chain bitset of one walk, and the entries of the
// directories fs.WalkDir holds open, which for a directory of n entries is about 300 bytes each.
const memoryCap = 8 << 20

func TestMemoryFixture(t *testing.T) {
	disk := loadImage(t, "jhfsplus-apm")
	off, size := partitionOf(t, disk)
	v, err := Open(bytes.NewReader(disk[off:off+size]), size, Options{CacheSize: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	h := newHeapSampler(1)
	n := walkMetadata(t, v, h.sample)
	t.Logf("%d entries, peak heap %d KiB above baseline with a 64 KiB cache", n, h.peak>>10)
	if h.peak > memoryCap {
		t.Errorf("peak heap %d bytes exceeds %d", h.peak, memoryCap)
	}
}

// TestMemoryBigVolume walks the 64 GiB image testdata/mkbig.sh makes, reading it from the file
// rather than memory, and checks the peak heap against the default cache size plus memoryCap.
func TestMemoryBigVolume(t *testing.T) {
	path := os.Getenv("HFSPLUS_BIG_IMAGE")
	if path == "" {
		t.Skip("HFSPLUS_BIG_IMAGE is not set; see testdata/mkbig.sh")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	off, size := gptHFSPartition(t, f)
	h := newHeapSampler(1000)
	v, err := Open(sectionOf(f, off, size), size, Options{})
	if err != nil {
		t.Fatal(err)
	}
	n := walkMetadata(t, v, h.sample)
	t.Logf("%d entries on a %d GiB volume, peak heap %.1f MiB above baseline with the default %d MiB cache",
		n, size>>30, float64(h.peak)/(1<<20), DefaultCacheSize>>20)
	if h.peak > DefaultCacheSize+memoryCap {
		t.Errorf("peak heap %d bytes exceeds %d", h.peak, DefaultCacheSize+memoryCap)
	}
}

func gptHFSPartition(t testing.TB, f io.ReaderAt) (int64, int64) {
	h := make([]byte, 512)
	if _, err := f.ReadAt(h, 512); err != nil || string(h[:8]) != "EFI PART" {
		t.Fatalf("no GPT: %v", err)
	}
	lba := int64(binary.LittleEndian.Uint64(h[72:]))
	n := int(binary.LittleEndian.Uint32(h[80:]))
	es := int64(binary.LittleEndian.Uint32(h[84:]))
	e := make([]byte, es)
	for i := range n {
		if _, err := f.ReadAt(e, lba*512+int64(i)*es); err != nil {
			t.Fatal(err)
		}
		if [16]byte(e[:16]) == hfsPlusGUID {
			first := int64(binary.LittleEndian.Uint64(e[32:]))
			last := int64(binary.LittleEndian.Uint64(e[40:]))
			return first * 512, (last - first + 1) * 512
		}
	}
	t.Fatal("no HFS+ partition")
	return 0, 0
}

func sectionOf(f *os.File, off, size int64) *sectionReaderAt {
	return &sectionReaderAt{f: f, off: off, size: size}
}

type sectionReaderAt struct {
	f         *os.File
	off, size int64
}

func (s *sectionReaderAt) ReadAt(p []byte, off int64) (int, error) {
	return s.f.ReadAt(p, s.off+off)
}
