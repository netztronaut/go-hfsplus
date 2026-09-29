package apm

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func loadImage(t testing.TB, name string) []byte {
	t.Helper()
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
	return b
}

func TestFixtures(t *testing.T) {
	for _, name := range []string{"hfsplus-apm", "jhfsplus-apm", "hfsx-apm", "jhfsx-apm"} {
		t.Run(name, func(t *testing.T) {
			disk := loadImage(t, name)
			m, err := Read(bytes.NewReader(disk), int64(len(disk)))
			if err != nil {
				t.Fatal(err)
			}
			if !m.HasDDR || m.BlockSize != 512 || int64(m.BlockCount)*512 != int64(len(disk)) {
				t.Errorf("DDR: %+v", m)
			}
			if m.EntrySize != 512 || m.Unit != 512 {
				t.Errorf("entry size %d, unit %d", m.EntrySize, m.Unit)
			}
			pm := m.Find(TypePartitionMap)
			if pm == nil || pm.StartBlock != 1 || pm.Index != 1 {
				t.Fatalf("partition map entry %+v", pm)
			}
			var hfs *Partition
			for i := range m.Partitions {
				p := &m.Partitions[i]
				t.Logf("%d %q %q start %d count %d status %#x", p.Index, p.Name, p.Type, p.StartBlock, p.BlockCount, p.Status)
				if p.Type == TypeHFS || p.Type == TypeHFSX {
					hfs = p
				}
			}
			if hfs == nil {
				t.Fatal("no HFS partition")
			}
			var sig [2]byte
			if _, err := hfs.Section().ReadAt(sig[:], 1024); err != nil || string(sig[:]) != "H+" && string(sig[:]) != "HX" {
				t.Errorf("partition does not start an HFS+ volume: %q %v", sig, err)
			}
			if hfs.Data(m).Size() != hfs.Length {
				t.Errorf("data area %d of %d", hfs.Data(m).Size(), hfs.Length)
			}
		})
	}
}

func TestNoMap(t *testing.T) {
	for _, name := range []string{"hfsplus-gpt"} {
		disk := loadImage(t, name)
		if _, err := Read(bytes.NewReader(disk), int64(len(disk))); !errors.Is(err, ErrNoMap) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, n := range []int{0, 512, 4096} {
		b := make([]byte, n)
		if _, err := Read(bytes.NewReader(b), int64(n)); !errors.Is(err, ErrNoMap) {
			t.Errorf("%d zero bytes: %v", n, err)
		}
	}
}

// entry writes a partition map entry.
func entry(b []byte, count, start, length uint32, name, typ string) {
	copy(b, "PM")
	binary.BigEndian.PutUint32(b[4:], count)
	binary.BigEndian.PutUint32(b[8:], start)
	binary.BigEndian.PutUint32(b[12:], length)
	copy(b[16:48], name)
	copy(b[48:80], typ)
	binary.BigEndian.PutUint32(b[84:], length)
	binary.BigEndian.PutUint32(b[88:], uint32(StatusValid|StatusAllocated|StatusReadable))
}

// synth builds a disk with a DDR of block size ddr, entries at stride, partition starts in unit.
func synth(size int64, ddr uint16, stride, unit int64, parts [][2]uint32, types []string) []byte {
	disk := make([]byte, size)
	copy(disk, "ER")
	binary.BigEndian.PutUint16(disk[2:], ddr)
	binary.BigEndian.PutUint32(disk[4:], uint32(size/int64(ddr)))
	n := uint32(len(parts))
	for i, p := range parts {
		entry(disk[int64(i+1)*stride:], n, p[0], p[1], "part", types[i])
	}
	_ = unit
	return disk
}

func TestOpticalBlockSizes(t *testing.T) {
	const size = 1 << 20
	// A 2048-byte DDR with entries every 2048 bytes and starts in 2048-byte blocks.
	d := synth(size, 2048, 2048, 2048, [][2]uint32{{1, 4}, {8, 100}}, []string{TypePartitionMap, TypeHFS})
	m, err := Read(bytes.NewReader(d), size)
	if err != nil {
		t.Fatal(err)
	}
	if m.EntrySize != 2048 || m.Unit != 2048 || m.Partitions[1].Start != 8*2048 {
		t.Errorf("2048/2048: entry size %d unit %d start %d", m.EntrySize, m.Unit, m.Partitions[1].Start)
	}
	// A 2048-byte DDR with the map at 512-byte strides and starts in 512-byte blocks, as some
	// Apple discs have it.
	d = synth(size, 2048, 512, 512, [][2]uint32{{1, 63}, {64, 400}}, []string{TypePartitionMap, TypeHFS})
	m, err = Read(bytes.NewReader(d), size)
	if err != nil {
		t.Fatal(err)
	}
	if m.EntrySize != 512 || m.Unit != 512 || m.Partitions[1].Start != 64*512 {
		t.Errorf("2048/512: entry size %d unit %d start %d", m.EntrySize, m.Unit, m.Partitions[1].Start)
	}
	// No DDR at all: still a map.
	clear(d[:512])
	if m, err = Read(bytes.NewReader(d), size); err != nil || m.HasDDR {
		t.Errorf("no DDR: %v, %+v", err, m)
	}
}

func TestInvalidEntries(t *testing.T) {
	const size = 1 << 20
	check := func(name string, d []byte, want error, index int) {
		t.Helper()
		_, err := Read(bytes.NewReader(d), size)
		var ee *EntryError
		if !errors.Is(err, want) || !errors.As(err, &ee) || ee.Index != index || !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
	check("outside the disk", synth(size, 512, 512, 512, [][2]uint32{{1, 3}, {100, 5000}}, []string{TypePartitionMap, TypeHFS}), ErrOutsideDisk, 2)
	// Trailing free space past the end, as a truncated disc image has it, is clipped.
	d0 := synth(size, 512, 512, 512, [][2]uint32{{1, 3}, {4, 2000}, {2004, 100}}, []string{TypePartitionMap, TypeHFS, TypeFree})
	if m, err := Read(bytes.NewReader(d0), size); err != nil || !m.Partitions[2].Truncated || m.Partitions[2].Start+m.Partitions[2].Length != size {
		t.Errorf("truncated trailing free space: %v", err)
	}
	check("overlaps the map", synth(size, 512, 512, 512, [][2]uint32{{1, 3}, {2, 100}}, []string{TypePartitionMap, TypeHFS}), ErrOverlapsMap, 2)
	d := synth(size, 512, 512, 512, [][2]uint32{{1, 1}, {10, 10}}, []string{TypePartitionMap, TypeHFS})
	check("more entries than the map holds", d, ErrTooManyEntries, 1)
	d = synth(size, 512, 512, 512, [][2]uint32{{1, 63}}, []string{TypePartitionMap})
	binary.BigEndian.PutUint32(d[512+4:], 1<<30)
	check("more entries than the disk holds", d, ErrTooManyEntries, 1)
	d = synth(size, 512, 512, 512, [][2]uint32{{1, 63}, {64, 10}}, []string{TypePartitionMap, TypeHFS})
	copy(d[1024:], "XX")
	check("bad signature", d, ErrCorrupt, 2)
}

func TestTypes(t *testing.T) {
	for _, typ := range []string{TypeDriver, TypeDriver43, TypeDriverATA, TypeFWDriver} {
		if !IsDriver(typ) {
			t.Errorf("%s is a driver", typ)
		}
	}
	if IsDriver(TypeHFS) || IsDriver(TypePatches) {
		t.Error("not drivers")
	}
	const size = 1 << 20
	d := synth(size, 512, 512, 512, [][2]uint32{{1, 63}, {64, 10}}, []string{TypePartitionMap, "Linux_Swap_Weird"})
	copy(d[1024+16:], "Caf\x8e") // Mac OS Roman é
	m, err := Read(bytes.NewReader(d), size)
	if err != nil {
		t.Fatal(err)
	}
	if m.Partitions[1].Type != "Linux_Swap_Weird" || m.Partitions[1].Name != "Café" {
		t.Errorf("type %q name %q", m.Partitions[1].Type, m.Partitions[1].Name)
	}
}

func FuzzRead(f *testing.F) {
	f.Add(synth(8192, 512, 512, 512, [][2]uint32{{1, 3}, {4, 12}}, []string{TypePartitionMap, TypeHFS}))
	f.Add(synth(16384, 2048, 2048, 2048, [][2]uint32{{1, 2}, {3, 4}}, []string{TypePartitionMap, TypeHFS}))
	f.Add(synth(16384, 2048, 512, 512, [][2]uint32{{1, 3}, {4, 20}}, []string{TypePartitionMap, TypeHFS}))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Read(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return
		}
		for _, p := range m.Partitions {
			if p.Start < 0 || p.Start+p.Length > int64(len(b)) {
				t.Fatalf("partition %d outside the disk", p.Index)
			}
			p.Section().ReadAt(make([]byte, 16), 0)
			p.Data(m).ReadAt(make([]byte, 16), 0)
		}
	})
}
