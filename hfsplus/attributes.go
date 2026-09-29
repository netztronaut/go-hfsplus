package hfsplus

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"unicode/utf16"
	"unicode/utf8"
)

// Attribute record types in the attributes B-tree.
const (
	attrInline  = 0x10 // kHFSPlusAttrInlineData
	attrFork    = 0x20 // kHFSPlusAttrForkData
	attrExtents = 0x30 // kHFSPlusAttrExtents

	maxAttrNameLen = 127
)

// Extended attribute names macOS synthesises or hides.
const (
	XattrFinderInfo   = "com.apple.FinderInfo"
	XattrResourceFork = "com.apple.ResourceFork"
	XattrDecmpfs      = "com.apple.decmpfs"
)

// attribute is one extended attribute as the attributes B-tree stores it.
type attribute struct {
	name   string
	inline []byte    // inline data, or nil
	fork   *ForkData // fork data, or nil
}

func (a *attribute) size() int64 {
	if a.fork != nil {
		return int64(a.fork.LogicalSize)
	}
	return int64(len(a.inline))
}

// attrKey decodes an attributes key: file ID, start block and name.
func attrKey(key []byte, buf *[maxAttrNameLen]uint16) (uint32, uint32, []uint16, error) {
	if len(key) < 12 {
		return 0, 0, nil, errBadKey
	}
	n := int(be16(key[10:]))
	if n > maxAttrNameLen || 12+2*n > len(key) {
		return 0, 0, nil, errBadKey
	}
	for i := range n {
		buf[i] = be16(key[12+2*i:])
	}
	return be32(key[2:]), be32(key[6:]), buf[:n], nil
}

func attrSearch(cnid uint32, name []uint16, start uint32) keyCompare {
	return func(key []byte) (int, error) {
		var buf [maxAttrNameLen]uint16
		id, sb, n, err := attrKey(key, &buf)
		if err != nil {
			return 0, err
		}
		if c := cmp.Compare(id, cnid); c != 0 {
			return c, nil
		}
		if c := compareBinary(n, name); c != 0 {
			return c, nil
		}
		return cmp.Compare(sb, start), nil
	}
}

// attrNameToHFS converts an attribute name: plain UTF-16, no decomposition, as xnu stores them.
func attrNameToHFS(s string) ([]uint16, error) {
	if !utf8.ValidString(s) || s == "" {
		return nil, fmt.Errorf("%w: attribute name %q", errInvalidName, s)
	}
	u := utf16.Encode([]rune(s))
	if len(u) > maxAttrNameLen {
		return nil, fmt.Errorf("%w: attribute name of %d units", errInvalidName, len(u))
	}
	return u, nil
}

func (v *volume) decodeAttr(n *node, i int, name string, data []byte) (*attribute, error) {
	if len(data) < 4 {
		return nil, corruptNode(v.attributes.name, n.num, "record %d is %d bytes", i, len(data))
	}
	a := &attribute{name: name}
	switch t := be32(data); t {
	case attrInline:
		if len(data) < 16 {
			return nil, corruptNode(v.attributes.name, n.num, "inline record %d is %d bytes", i, len(data))
		}
		size := int(be32(data[12:]))
		if size > len(data)-16 {
			return nil, corruptNode(v.attributes.name, n.num, "inline record %d claims %d bytes in %d", i, size, len(data)-16)
		}
		a.inline = data[16 : 16+size : 16+size]
	case attrFork:
		if len(data) < 88 {
			return nil, corruptNode(v.attributes.name, n.num, "fork record %d is %d bytes", i, len(data))
		}
		fd := decodeFork(data[8:])
		a.fork = &fd
	default:
		return nil, corruptNode(v.attributes.name, n.num, "record %d of type %#x at start block 0", i, t)
	}
	return a, nil
}

// attrs lists the attributes of cnid in the attributes B-tree, in B-tree order.
func (v *volume) attrs(ctx context.Context, cnid uint32) ([]*attribute, error) {
	if v.attributes == nil {
		return nil, nil
	}
	n, i, _, err := v.attributes.search(ctx, attrSearch(cnid, nil, 0))
	if err != nil || n == nil {
		return nil, err
	}
	// Record i sorts before (cnid, "", 0), which no attribute has; the walk starts after it.
	w := v.attributes.walkFrom(n, i)
	var out []*attribute
	for {
		key, data, ok, err := w.next(ctx)
		if err != nil || !ok {
			return out, err
		}
		var buf [maxAttrNameLen]uint16
		id, start, name, err := attrKey(key, &buf)
		if err != nil {
			return nil, corruptNode(v.attributes.name, w.n.num, "record %d: %v", w.i, err)
		}
		if id < cnid {
			continue
		}
		if id > cnid {
			return out, nil
		}
		if start != 0 {
			continue // extents of a fork attribute
		}
		a, err := v.decodeAttr(w.n, w.i, string(utf16.Decode(name)), data)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
}

// attr returns the attribute name of cnid, or nil.
func (v *volume) attr(ctx context.Context, cnid uint32, name string) (*attribute, error) {
	if v.attributes == nil {
		return nil, nil
	}
	u, err := attrNameToHFS(name)
	if err != nil {
		return nil, err
	}
	n, i, exact, err := v.attributes.search(ctx, attrSearch(cnid, u, 0))
	if err != nil || !exact {
		return nil, err
	}
	_, data, err := v.attributes.key(n, i)
	if err != nil {
		return nil, err
	}
	return v.decodeAttr(n, i, name, data)
}

// attrReader returns a reader of an attribute's value.
func (v *volume) attrReader(ctx context.Context, cnid uint32, a *attribute) (io.ReaderAt, error) {
	if a.fork == nil {
		return bytesReaderAt(a.inline), nil
	}
	u, _ := attrNameToHFS(a.name)
	f, err := v.newFork(fmt.Sprintf("attribute %q of CNID %d", a.name, cnid), *a.fork, func(ctx context.Context, start uint32) (*[8]Extent, error) {
		n, i, exact, err := v.attributes.search(ctx, attrSearch(cnid, u, start))
		if err != nil || !exact {
			return nil, err
		}
		_, data, err := v.attributes.key(n, i)
		if err != nil {
			return nil, err
		}
		if len(data) < 72 || be32(data) != attrExtents {
			return nil, corruptNode(v.attributes.name, n.num, "record %d is not an extents record", i)
		}
		var e [8]Extent
		decodeExtents(&e, data[8:])
		return &e, nil
	})
	if err != nil {
		return nil, err
	}
	return forkReader{ctx: ctx, f: f}, nil
}

// readAttr reads an attribute's whole value, refusing one larger than limit.
func (v *volume) readAttr(ctx context.Context, cnid uint32, a *attribute, limit int64) ([]byte, error) {
	if a.fork == nil {
		return append([]byte(nil), a.inline...), nil
	}
	if a.size() > limit {
		return nil, corrupt("attributes B-tree", -1, "attribute %q of CNID %d is %d bytes, more than %d", a.name, cnid, a.size(), limit)
	}
	r, err := v.attrReader(ctx, cnid, a)
	if err != nil {
		return nil, err
	}
	b := make([]byte, a.size())
	if _, err := r.ReadAt(b, 0); err != nil && err != io.EOF {
		return nil, err
	}
	return b, nil
}

type bytesReaderAt []byte

func (b bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errInvalidOffset
	}
	if off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
