package main

// A throwaway, minimal HFS+ catalog reader used only by the measurement step:
// it walks the leaf nodes of the catalog B-tree of a bare (layout NONE) image
// and returns every record's key in on-disk order. It deliberately does not
// share code with the hfsplus package so that it measures, not assumes.

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

type catRecord struct {
	ParentID uint32
	Name     []uint16
	Type     int16  // 1 folder, 2 file, 3 folder thread, 4 file thread
	CNID     uint32 // for folder and file records
	NameOff  int64  // absolute image offset of the first name unit
	Node     uint32 // leaf node number
	Index    int    // record index within the node
	NodeRecs int    // number of records in the node
}

type catalog struct {
	Signature      string
	KeyCompareType uint8
	NodeSize       int
	Records        []catRecord
}

type extent struct{ start, count uint32 }

type forkMap struct {
	blockSize int64
	extents   []extent
}

// offset maps a byte offset within the fork to an image offset.
func (f *forkMap) offset(off int64) (int64, error) {
	blk := off / f.blockSize
	for _, e := range f.extents {
		if blk < int64(e.count) {
			return (int64(e.start)+blk)*f.blockSize + off%f.blockSize, nil
		}
		blk -= int64(e.count)
	}
	return 0, fmt.Errorf("offset %d beyond fork extents", off)
}

func parseExtents(b []byte) []extent {
	var out []extent
	for i := 0; i < 8; i++ {
		s := binary.BigEndian.Uint32(b[8*i:])
		n := binary.BigEndian.Uint32(b[8*i+4:])
		if n != 0 {
			out = append(out, extent{s, n})
		}
	}
	return out
}

// readNode reads node n of a B-tree stored in fork f. It returns the node and
// the image offset of its first byte for each block-contiguous piece; nodes
// are assumed not to straddle discontiguous extents at an unaligned point,
// which holds whenever nodeSize <= blockSize or extents are node-aligned.
func readNode(r io.ReaderAt, f *forkMap, n uint32, nodeSize int) ([]byte, int64, error) {
	off := int64(n) * int64(nodeSize)
	buf := make([]byte, nodeSize)
	base, err := f.offset(off)
	if err != nil {
		return nil, 0, err
	}
	for i := 0; i < nodeSize; {
		chunk := int(f.blockSize - (off+int64(i))%f.blockSize)
		if chunk > nodeSize-i {
			chunk = nodeSize - i
		}
		o, err := f.offset(off + int64(i))
		if err != nil {
			return nil, 0, err
		}
		if _, err := r.ReadAt(buf[i:i+chunk], o); err != nil {
			return nil, 0, err
		}
		i += chunk
	}
	return buf, base, nil
}

type btHeader struct {
	firstLeaf uint32
	nodeSize  int
	kct       uint8
}

func readHeader(r io.ReaderAt, f *forkMap) (btHeader, error) {
	b := make([]byte, 256)
	o, err := f.offset(0)
	if err != nil {
		return btHeader{}, err
	}
	if _, err := r.ReadAt(b, o); err != nil {
		return btHeader{}, err
	}
	h := b[14:]
	return btHeader{
		firstLeaf: binary.BigEndian.Uint32(h[10:]),
		nodeSize:  int(binary.BigEndian.Uint16(h[18:])),
		kct:       h[37],
	}, nil
}

// leafRecords calls fn for every record of every leaf node in order.
func leafRecords(r io.ReaderAt, f *forkMap, fn func(node []byte, nodeOff int64, rec int) error) (btHeader, error) {
	return leafRecordsN(r, f, func(_ uint32, nd []byte, off int64, rec, _, _ int) error { return fn(nd, off, rec) })
}

// leafRecordsN is leafRecords also passing the node number, record index and count.
func leafRecordsN(r io.ReaderAt, f *forkMap, fn func(n uint32, node []byte, nodeOff int64, rec, idx, nrec int) error) (btHeader, error) {
	h, err := readHeader(r, f)
	if err != nil {
		return h, err
	}
	seen := map[uint32]bool{}
	for n := h.firstLeaf; n != 0; {
		if seen[n] {
			return h, fmt.Errorf("leaf loop at node %d", n)
		}
		seen[n] = true
		nd, base, err := readNode(r, f, n, h.nodeSize)
		if err != nil {
			return h, err
		}
		if int8(nd[8]) != -1 {
			return h, fmt.Errorf("node %d is not a leaf (kind %d)", n, int8(nd[8]))
		}
		nrec := int(binary.BigEndian.Uint16(nd[10:]))
		for i := 0; i < nrec; i++ {
			ro := int(binary.BigEndian.Uint16(nd[h.nodeSize-2*(i+1):]))
			if err := fn(n, nd, base, ro, i, nrec); err != nil {
				return h, err
			}
		}
		n = binary.BigEndian.Uint32(nd[0:])
	}
	return h, nil
}

// readCatalog reads the catalog of the bare HFS+/HFSX image at path.
func readCatalog(path string) (*catalog, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	vh := make([]byte, 512)
	if _, err := fh.ReadAt(vh, 1024); err != nil {
		return nil, err
	}
	sig := string(vh[:2])
	if sig != "H+" && sig != "HX" {
		return nil, fmt.Errorf("%s: no HFS+ volume header at 1024 (%q)", path, sig)
	}
	bs := int64(binary.BigEndian.Uint32(vh[40:]))
	extFork := &forkMap{bs, parseExtents(vh[0xC0+16:])}
	catFork := &forkMap{bs, parseExtents(vh[0x110+16:])}
	catBlocks := binary.BigEndian.Uint32(vh[0x110+12:])
	var have uint32
	for _, e := range catFork.extents {
		have += e.count
	}
	if have < catBlocks {
		// Catalog extents overflow: collect the catalog's (fileID 4, data fork) records.
		var more []extent
		_, err := leafRecords(fh, extFork, func(nd []byte, _ int64, o int) error {
			forkType := nd[o+2]
			fileID := binary.BigEndian.Uint32(nd[o+4:])
			if fileID == 4 && forkType == 0 {
				more = append(more, parseExtents(nd[o+12:])...)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("extents overflow: %w", err)
		}
		catFork.extents = append(catFork.extents, more...)
	}
	c := &catalog{Signature: sig}
	h, err := leafRecordsN(fh, catFork, func(node uint32, nd []byte, base int64, o, idx, nrec int) error {
		kl := int(binary.BigEndian.Uint16(nd[o:]))
		pid := binary.BigEndian.Uint32(nd[o+2:])
		nl := int(binary.BigEndian.Uint16(nd[o+6:]))
		name := make([]uint16, nl)
		for i := range name {
			name[i] = binary.BigEndian.Uint16(nd[o+8+2*i:])
		}
		ro := o + 2 + kl
		rt := int16(binary.BigEndian.Uint16(nd[ro:]))
		var cnid uint32
		if rt == 1 || rt == 2 {
			cnid = binary.BigEndian.Uint32(nd[ro+8:])
		}
		// Image offset of the name, valid when the node lies in one extent piece
		// (always true here: node size <= block size or contiguous catalog).
		c.Records = append(c.Records, catRecord{pid, name, rt, cnid, base + int64(o+8), node, idx, nrec})
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.KeyCompareType = h.kct
	c.NodeSize = h.nodeSize
	return c, nil
}

// children returns the file and folder records whose parent is dirID, in
// on-disk (key) order.
func (c *catalog) children(dirID uint32) []catRecord {
	var out []catRecord
	for _, r := range c.Records {
		if r.ParentID == dirID && (r.Type == 1 || r.Type == 2) {
			out = append(out, r)
		}
	}
	return out
}
