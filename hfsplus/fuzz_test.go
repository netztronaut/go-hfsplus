package hfsplus

import (
	"bytes"
	"encoding/binary"
	"io"
	"io/fs"
	"testing"
)

// The fuzz targets put the fuzzer's bytes into one structure of a real volume and read the whole
// volume through it: whatever the bytes, the reader returns errors, never panics or hangs.

// exercise walks the volume and reads everything it can, ignoring errors.
func exercise(v *Volume) {
	v.Blessed()
	v.Name()
	n := 0
	fs.WalkDir(v, ".", func(p string, d fs.DirEntry, err error) error {
		if n++; n > 2000 {
			return fs.SkipAll
		}
		if err != nil {
			return nil
		}
		v.Lstat(p)
		v.Stat(p)
		if names, err := v.ListXattr(p); err == nil {
			for _, x := range names {
				v.GetXattr(p, x)
			}
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			v.ReadLink(p)
		case d.Type().IsRegular():
			if f, err := v.Open(p); err == nil {
				io.Copy(io.Discard, io.LimitReader(f.(io.Reader), 1<<20))
				f.Close()
			}
		}
		return nil
	})
}

func fuzzPartition(f *testing.F, name string) ([]byte, int64) {
	disk := loadImage(f, name)
	off, size := partitionOf(f, disk)
	return disk[off : off+size], size
}

// FuzzVolumeHeader fuzzes the 512 bytes at offset 1024: the volume header, or an HFS Master
// Directory Block wrapping one.
func FuzzVolumeHeader(f *testing.F) {
	part, size := fuzzPartition(f, "jhfsplus-apm")
	f.Add(part[headerOffset : headerOffset+headerSize])
	mdb := make([]byte, headerSize)
	copy(mdb, "BD")
	binary.BigEndian.PutUint32(mdb[20:], 4096)
	copy(mdb[124:], "H+")
	binary.BigEndian.PutUint16(mdb[128:], 100)
	f.Add(mdb)
	f.Fuzz(func(t *testing.T, b []byte) {
		p := bytes.Clone(part)
		copy(p[headerOffset:headerOffset+headerSize], b)
		v, err := Open(bytes.NewReader(p), size, Options{CacheSize: 64 << 10})
		if err != nil {
			return
		}
		exercise(v)
	})
}

// catalogNode returns the byte offset in the partition of catalog node n, which the fixtures keep
// in the catalog's first extent.
func catalogNode(part []byte, n int) (int64, int) {
	h := decodeVolumeHeader(part[headerOffset:])
	start := int64(h.CatalogFile.Extents[0].StartBlock) * int64(h.BlockSize)
	nodeSize := int(be16(part[start+nodeDescriptorSize+18:]))
	return start + int64(n)*int64(nodeSize), nodeSize
}

// FuzzBTreeNode fuzzes a catalog node: the header node, the root, or a leaf.
func FuzzBTreeNode(f *testing.F) {
	part, size := fuzzPartition(f, "hfsplus-apm")
	hdrOff, nodeSize := catalogNode(part, 0)
	root := int(be32(part[hdrOff+nodeDescriptorSize+2:]))
	first := int(be32(part[hdrOff+nodeDescriptorSize+10:]))
	for _, n := range []int{0, root, first} {
		off, _ := catalogNode(part, n)
		f.Add(uint8(n), part[off:off+int64(nodeSize)])
	}
	f.Fuzz(func(t *testing.T, which uint8, b []byte) {
		n := []int{0, root, first}[int(which)%3]
		off, _ := catalogNode(part, n)
		p := bytes.Clone(part)
		copy(p[off:off+int64(nodeSize)], b)
		v, err := Open(bytes.NewReader(p), size, Options{CacheSize: 64 << 10})
		if err != nil {
			return
		}
		exercise(v)
	})
}

// FuzzJournal fuzzes the journal header and the first block list header of a dirty journal, and
// replays it.
func FuzzJournal(f *testing.F) {
	part, size := fuzzPartition(f, "dirty-jhfsplus-apm")
	h := decodeVolumeHeader(part[headerOffset:])
	jib := int64(h.JournalInfoBlock) * int64(h.BlockSize)
	jOff := int64(be64(part[jib+36:]))
	start := int64(binary.LittleEndian.Uint64(part[jOff+8:]))
	f.Add(part[jOff:jOff+512], part[jOff+start:jOff+start+512])
	f.Fuzz(func(t *testing.T, hdr, blhdr []byte) {
		p := bytes.Clone(part)
		copy(p[jOff:jOff+512], hdr)
		copy(p[jOff+start:jOff+start+512], blhdr)
		v, err := Open(bytes.NewReader(p), size, Options{CacheSize: 64 << 10, MaxJournalBlocks: 1 << 16})
		if err != nil {
			return
		}
		exercise(v)
	})
}

// FuzzDecmpfs fuzzes a com.apple.decmpfs attribute and a resource fork, and reads the file they
// make.
func FuzzDecmpfs(f *testing.F) {
	v := openImage(f, "jhfsplus-apm", Options{})
	fs.WalkDir(v, "compressed", func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		r, _ := v.Record(p)
		if !r.Compressed() {
			return nil
		}
		a, _ := v.attr(v.ctx, r.CNID, XattrDecmpfs)
		raw, _ := v.readAttr(v.ctx, r.CNID, a, maxDecmpfsAttr)
		rf, _ := v.OpenResourceFork(p)
		rsrc, _ := io.ReadAll(io.LimitReader(rf, 1<<16))
		f.Add(raw, rsrc)
		return nil
	})
	f.Fuzz(func(t *testing.T, attr, rsrc []byte) {
		h, err := decodeDecmpfs(attr)
		if err != nil {
			return
		}
		d := &decmpfsFile{v: v.volume, hdr: h, size: int64(min(h.UncompressedSize, 1<<62)), cached: -1, dec: v.decompressors[h.Type]}
		if h.Type != 1 && d.dec == nil {
			return
		}
		switch {
		case decmpfsInline(h.Type):
			if d.size > 1<<20 {
				return
			}
			d.data = make([]byte, d.size)
			if h.Type == 1 || d.size == 0 || len(attr) == decmpfsHeaderSize {
				return
			}
			if err := d.decode(d.data, attr[decmpfsHeaderSize:]); err != nil {
				return
			}
		case decmpfsInRsrc(h.Type):
			d.rsrc = bytesReaderAt(rsrc)
			d.rsrcLen = int64(len(rsrc))
			d.nchunks = (d.size + decmpfsChunkSize - 1) / decmpfsChunkSize
			if err := d.readTable(); err != nil {
				return
			}
		default:
			return
		}
		buf := make([]byte, 100000)
		for off := int64(0); off < min(d.size, 4<<20); off += int64(len(buf)) {
			if _, err := d.ReadAt(buf, off); err != nil {
				return
			}
		}
	})
}
