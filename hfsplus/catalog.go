package hfsplus

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
)

// maxPathDepth bounds the parent chain PathOf follows.
const maxPathDepth = 4096

var errBadKey = errors.New("malformed key")

// catalogKey decodes a catalog key into its parent ID and name, the name into buf.
func catalogKey(key []byte, buf *[255]uint16) (uint32, []uint16, error) {
	if len(key) < 6 {
		return 0, nil, errBadKey
	}
	n := int(be16(key[4:]))
	if n > 255 || 6+2*n > len(key) {
		return 0, nil, errBadKey
	}
	for i := range n {
		buf[i] = be16(key[6+2*i:])
	}
	return be32(key), buf[:n], nil
}

func (v *volume) compareNames(a, b []uint16) int {
	if v.caseSensitive {
		return compareBinary(a, b)
	}
	return compareFold(a, b)
}

// catalogSearch compares catalog keys with (parent, name).
func (v *volume) catalogSearch(parent uint32, name []uint16) keyCompare {
	return func(key []byte) (int, error) {
		var buf [255]uint16
		p, n, err := catalogKey(key, &buf)
		if err != nil {
			return 0, err
		}
		if p != parent {
			return cmp.Compare(p, parent), nil
		}
		return v.compareNames(n, name), nil
	}
}

// lookup returns the folder or file record named name in the folder parent, or nil when there is
// none.
func (v *volume) lookup(ctx context.Context, parent uint32, name []uint16) (*Record, error) {
	n, i, exact, err := v.catalog.search(ctx, v.catalogSearch(parent, name))
	if err != nil || !exact {
		return nil, err
	}
	key, data, err := v.catalog.key(n, i)
	if err != nil {
		return nil, err
	}
	r, ok, err := decodeRecord(data)
	if err != nil {
		return nil, corruptNode(v.catalog.name, n.num, "record %d: %v", i, err)
	}
	if !ok {
		// A thread record's key has an empty name; a folder or file never does.
		return nil, corruptNode(v.catalog.name, n.num, "record %d: type %d under a named key", i, int16(be16(data)))
	}
	var buf [255]uint16
	_, stored, _ := catalogKey(key, &buf)
	r.ParentID = parent
	r.Name = nameFromHFS(stored)
	return r, nil
}

// thread returns the parent and name of the folder or file cnid, from its thread record.
func (v *volume) thread(ctx context.Context, cnid uint32) (parent uint32, name []uint16, folder bool, err error) {
	n, i, exact, err := v.catalog.search(ctx, v.catalogSearch(cnid, nil))
	if err != nil {
		return 0, nil, false, err
	}
	if !exact {
		return 0, nil, false, fs.ErrNotExist
	}
	_, data, err := v.catalog.key(n, i)
	if err != nil {
		return 0, nil, false, err
	}
	if len(data) < 10 {
		return 0, nil, false, corruptNode(v.catalog.name, n.num, "thread record %d is %d bytes", i, len(data))
	}
	typ := int16(be16(data))
	if typ != recFolderThread && typ != recFileThread {
		return 0, nil, false, corruptNode(v.catalog.name, n.num, "record %d: type %d under a thread key", i, typ)
	}
	l := int(be16(data[8:]))
	if l > 255 || 10+2*l > len(data) {
		return 0, nil, false, corruptNode(v.catalog.name, n.num, "thread record %d name of %d units", i, l)
	}
	name = make([]uint16, l)
	for k := range l {
		name[k] = be16(data[10+2*k:])
	}
	return be32(data[4:]), name, typ == recFolderThread, nil
}

// byID returns the record of cnid.
func (v *volume) byID(ctx context.Context, cnid uint32) (*Record, error) {
	parent, name, _, err := v.thread(ctx, cnid)
	if err != nil {
		return nil, err
	}
	r, err := v.lookup(ctx, parent, name)
	if err != nil {
		return nil, err
	}
	if r == nil || r.CNID != cnid {
		return nil, corrupt("catalog B-tree", -1, "thread of CNID %d names a record that is not there", cnid)
	}
	return r, nil
}

// PathOf returns the path of the folder or file cnid, relative to the volume root ("." for the
// root), by following thread records up the parent chain. It returns an error wrapping
// fs.ErrNotExist when there is no such CNID.
func (v *Volume) PathOf(cnid uint32) (string, error) {
	if cnid == CNIDRootFolder {
		return ".", nil
	}
	var parts []string
	seen := map[uint32]bool{}
	for id := cnid; id != CNIDRootFolder; {
		if len(parts) >= maxPathDepth || seen[id] {
			return "", corrupt("catalog B-tree", -1, "the parent chain of CNID %d does not reach the root", cnid)
		}
		seen[id] = true
		parent, name, _, err := v.thread(v.ctx, id)
		if err != nil {
			if isNotExist(err) {
				return "", fmt.Errorf("hfsplus: CNID %d: %w", cnid, fs.ErrNotExist)
			}
			return "", err
		}
		if parent == CNIDRootParent {
			return "", corrupt("catalog B-tree", -1, "CNID %d is not under the root folder", cnid)
		}
		parts = append(parts, nameFromHFS(name))
		id = parent
	}
	slices.Reverse(parts)
	p := parts[0]
	for _, s := range parts[1:] {
		p += "/" + s
	}
	return p, nil
}

// dirIter iterates the records of one folder, in catalog order.
type dirIter struct {
	v      *volume
	parent uint32
	w      *walker
	done   bool
}

func (v *volume) list(ctx context.Context, parent uint32) (*dirIter, error) {
	n, i, _, err := v.catalog.search(ctx, v.catalogSearch(parent, nil))
	if err != nil {
		return nil, err
	}
	it := &dirIter{v: v, parent: parent}
	if n == nil {
		it.done = true
		return it, nil
	}
	// Record i is the folder's thread record, or the last record before where it would be; the
	// walk starts after it.
	it.w = v.catalog.walkFrom(n, i)
	return it, nil
}

// next returns the next folder or file record, or nil at the end of the folder.
func (it *dirIter) next(ctx context.Context) (*Record, error) {
	for !it.done {
		key, data, ok, err := it.w.next(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			it.done = true
			break
		}
		var buf [255]uint16
		p, name, err := catalogKey(key, &buf)
		if err != nil {
			return nil, corruptNode(it.v.catalog.name, it.w.n.num, "record %d: %v", it.w.i, err)
		}
		if p < it.parent {
			continue
		}
		if p > it.parent {
			it.done = true
			break
		}
		r, ok, err := decodeRecord(data)
		if err != nil {
			return nil, corruptNode(it.v.catalog.name, it.w.n.num, "record %d: %v", it.w.i, err)
		}
		if !ok {
			continue // the folder's thread record
		}
		r.ParentID = p
		r.Name = nameFromHFS(name)
		return r, nil
	}
	return nil, nil
}

// overflow returns the lookup of a fork's extents beyond its first eight in the extents overflow
// B-tree.
func (v *volume) overflow(cnid uint32, forkType uint8) func(context.Context, uint32) (*[8]Extent, error) {
	return func(ctx context.Context, start uint32) (*[8]Extent, error) {
		n, i, exact, err := v.extents.search(ctx, func(key []byte) (int, error) {
			if len(key) < 10 {
				return 0, errBadKey
			}
			if c := cmp.Compare(be32(key[2:]), cnid); c != 0 {
				return c, nil
			}
			if c := cmp.Compare(key[0], forkType); c != 0 {
				return c, nil
			}
			return cmp.Compare(be32(key[6:]), start), nil
		})
		if err != nil || !exact {
			return nil, err
		}
		_, data, err := v.extents.key(n, i)
		if err != nil {
			return nil, err
		}
		if len(data) < 64 {
			return nil, corruptNode(v.extents.name, n.num, "extent record %d is %d bytes", i, len(data))
		}
		var e [8]Extent
		decodeExtents(&e, data)
		return &e, nil
	}
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
