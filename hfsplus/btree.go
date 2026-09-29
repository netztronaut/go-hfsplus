package hfsplus

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

// B-trees, as TN1150 describes them: fixed-size nodes, node 0 the header node, index nodes
// pointing down to leaf nodes that are chained left to right.

const (
	kindLeaf   = -1
	kindIndex  = 0
	kindHeader = 1
	kindMap    = 2

	nodeDescriptorSize = 14
	// maxTreeDepth is kMaxTreeDepth from Apple's BTreesPrivate.h.
	maxTreeDepth = 16

	btBigKeys           = 0x2 // kBTBigKeysMask
	btVariableIndexKeys = 0x4 // kBTVariableIndexKeysMask

	// Key compare types in the header record of an HFSX catalog.
	keyCompareFolding = 0xCF
	keyCompareBinary  = 0xBC
)

var (
	errShortRecord   = errors.New("record shorter than its type")
	errInvalidOffset = errors.New("invalid offset")
)

// btree is one of the volume's B-trees.
type btree struct {
	v    *volume
	id   int    // cache key
	name string // "catalog B-tree", ...
	f    *fork

	nodeSize    int
	depth       int
	root        uint32
	firstLeaf   uint32
	lastLeaf    uint32
	totalNodes  uint32
	leafRecords uint32
	maxKeyLen   int
	attributes  uint32
	compareType uint8
	btreeType   uint8
}

// node is a validated B-tree node.
type node struct {
	num    uint32
	data   []byte
	fLink  uint32
	bLink  uint32
	kind   int8
	height uint8
	offs   []uint16 // numRecords+1 record offsets, ascending; the last is the free space
}

func (n *node) numRecords() int { return len(n.offs) - 1 }

// record returns record i's bytes.
func (n *node) record(i int) []byte { return n.data[n.offs[i]:n.offs[i+1]] }

func (v *volume) openBTree(ctx context.Context, id int, name string, f *fork) (*btree, error) {
	t := &btree{v: v, id: id, name: name, f: f}
	var hdr [nodeDescriptorSize + 106]byte
	if _, err := f.readAt(ctx, hdr[:], 0); err != nil {
		return nil, corruptNode(name, 0, "reading the header node: %v", err)
	}
	if int8(hdr[8]) != kindHeader {
		return nil, corruptNode(name, 0, "node 0 is kind %d, not a header node", int8(hdr[8]))
	}
	h := hdr[nodeDescriptorSize:]
	t.depth = int(be16(h[0:]))
	t.root = be32(h[2:])
	t.leafRecords = be32(h[6:])
	t.firstLeaf = be32(h[10:])
	t.lastLeaf = be32(h[14:])
	t.nodeSize = int(be16(h[18:]))
	t.maxKeyLen = int(be16(h[20:]))
	t.totalNodes = be32(h[22:])
	t.btreeType = h[36]
	t.compareType = h[37]
	t.attributes = be32(h[38:])
	switch {
	case t.nodeSize < 512 || t.nodeSize > 32768 || t.nodeSize&(t.nodeSize-1) != 0:
		return nil, corruptNode(name, 0, "node size %d", t.nodeSize)
	case uint64(t.totalNodes)*uint64(t.nodeSize) > uint64(f.size):
		return nil, corruptNode(name, 0, "%d nodes of %d bytes exceed the %d-byte fork", t.totalNodes, t.nodeSize, f.size)
	case t.depth > maxTreeDepth:
		return nil, corruptNode(name, 0, "depth %d exceeds %d", t.depth, maxTreeDepth)
	case t.depth > 0 && (t.root == 0 || t.root >= t.totalNodes):
		return nil, corruptNode(name, 0, "root node %d of %d", t.root, t.totalNodes)
	case t.attributes&btBigKeys == 0:
		return nil, corruptNode(name, 0, "attributes %#x lack big keys", t.attributes)
	case t.maxKeyLen < 6 || t.maxKeyLen > t.nodeSize/2:
		return nil, corruptNode(name, 0, "maximum key length %d", t.maxKeyLen)
	}
	return t, nil
}

// node reads, validates and caches node num.
func (t *btree) node(ctx context.Context, num uint32) (*node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if num >= t.totalNodes {
		return nil, corruptNode(t.name, num, "node number beyond the tree's %d nodes", t.totalNodes)
	}
	if n := t.v.cache.get(t.id, num); n != nil {
		return n, nil
	}
	data := make([]byte, t.nodeSize)
	if _, err := t.f.readAt(ctx, data, int64(num)*int64(t.nodeSize)); err != nil {
		var ce *CorruptError
		if errors.As(err, &ce) || ctx.Err() != nil {
			return nil, err
		}
		return nil, corruptNode(t.name, num, "reading: %v", err)
	}
	n := &node{
		num:    num,
		data:   data,
		fLink:  be32(data[0:]),
		bLink:  be32(data[4:]),
		kind:   int8(data[8]),
		height: data[9],
	}
	nrec := int(be16(data[10:]))
	if 2*(nrec+1) > t.nodeSize-nodeDescriptorSize {
		return nil, corruptNode(t.name, num, "%d records do not fit", nrec)
	}
	switch n.kind {
	case kindLeaf:
		if n.height != 1 {
			return nil, corruptNode(t.name, num, "leaf node at height %d", n.height)
		}
	case kindIndex:
		if n.height < 2 {
			return nil, corruptNode(t.name, num, "index node at height %d", n.height)
		}
	case kindHeader, kindMap:
	default:
		return nil, corruptNode(t.name, num, "node kind %d", n.kind)
	}
	n.offs = make([]uint16, nrec+1)
	limit := t.nodeSize - 2*(nrec+1)
	prev := nodeDescriptorSize
	for i := 0; i <= nrec; i++ {
		o := int(be16(data[t.nodeSize-2*(i+1):]))
		if o < prev || o > limit || (i > 0 && o == prev) {
			return nil, corruptNode(t.name, num, "record %d at offset %d (previous %d, limit %d)", i, o, prev, limit)
		}
		n.offs[i] = uint16(o)
		prev = o
	}
	t.v.cache.put(t.id, num, n, t.nodeSize)
	return n, nil
}

// key splits record i of an index or leaf node into its key (without the key length) and the data
// that follows it.
func (t *btree) key(n *node, i int) (key, data []byte, err error) {
	rec := n.record(i)
	if len(rec) < 2 {
		return nil, nil, corruptNode(t.name, n.num, "record %d is %d bytes", i, len(rec))
	}
	kl := int(be16(rec))
	if kl > t.maxKeyLen {
		return nil, nil, corruptNode(t.name, n.num, "record %d key length %d exceeds %d", i, kl, t.maxKeyLen)
	}
	keyEnd := 2 + kl
	if n.kind == kindIndex && t.attributes&btVariableIndexKeys == 0 {
		keyEnd = 2 + t.maxKeyLen
	}
	keyEnd += keyEnd & 1
	if keyEnd > len(rec) {
		return nil, nil, corruptNode(t.name, n.num, "record %d key of %d bytes in a %d-byte record", i, kl, len(rec))
	}
	return rec[2 : 2+kl], rec[keyEnd:], nil
}

// child returns the node an index record points to.
func (t *btree) child(n *node, i int) (uint32, error) {
	_, data, err := t.key(n, i)
	if err != nil {
		return 0, err
	}
	if len(data) < 4 {
		return 0, corruptNode(t.name, n.num, "index record %d has no child pointer", i)
	}
	return be32(data), nil
}

// keyCompare compares a record's key with the key being searched for: <0, 0 or >0 as the record's
// key sorts before, equal to or after it. It returns an error for a key it cannot decode.
type keyCompare func(key []byte) (int, error)

// search descends to the leaf holding the last record whose key is at or before the search key.
// It returns that leaf, the record's index (-1 when every key in the tree sorts after the search
// key, in which case the leaf is the first one) and whether the key is equal.
func (t *btree) search(ctx context.Context, cmp keyCompare) (*node, int, bool, error) {
	if t.depth == 0 || t.root == 0 {
		return nil, -1, false, nil
	}
	num := t.root
	height := t.depth
	for level := 0; ; level++ {
		if level >= maxTreeDepth || level >= t.depth {
			return nil, 0, false, corruptNode(t.name, num, "deeper than the tree's depth %d", t.depth)
		}
		n, err := t.node(ctx, num)
		if err != nil {
			return nil, 0, false, err
		}
		if int(n.height) != height {
			return nil, 0, false, corruptNode(t.name, num, "height %d, expected %d", n.height, height)
		}
		if (height == 1) != (n.kind == kindLeaf) || (height > 1 && n.kind != kindIndex) {
			return nil, 0, false, corruptNode(t.name, num, "kind %d at height %d", n.kind, height)
		}
		if n.numRecords() == 0 {
			return nil, 0, false, corruptNode(t.name, num, "empty %s node", map[bool]string{true: "leaf", false: "index"}[n.kind == kindLeaf])
		}
		// Binary search for the last record whose key is <= the search key.
		lo, hi := 0, n.numRecords() // invariant: records < lo are <=, records >= hi are >
		exact := false
		for lo < hi {
			mid := int(uint(lo+hi) >> 1)
			k, _, err := t.key(n, mid)
			if err != nil {
				return nil, 0, false, err
			}
			c, err := cmp(k)
			if err != nil {
				return nil, 0, false, corruptNode(t.name, num, "record %d: %v", mid, err)
			}
			if c <= 0 {
				lo = mid + 1
				exact = c == 0
				if exact {
					break
				}
			} else {
				hi = mid
			}
		}
		idx := lo - 1
		if n.kind == kindLeaf {
			return n, idx, exact, nil
		}
		if idx < 0 {
			// The search key sorts before the whole subtree: follow the first child, so that a
			// caller walking forward starts from the first leaf.
			idx = 0
		}
		child, err := t.child(n, idx)
		if err != nil {
			return nil, 0, false, err
		}
		num = child
		height--
	}
}

// walker iterates leaf records forward, following fLink, and detects cycles.
type walker struct {
	t       *btree
	n       *node
	i       int
	visited []uint64 // a bit per node, allocated when the walk leaves its first leaf
}

// walkFrom starts a walk at record i of leaf n (i may be -1, meaning before the first record).
func (t *btree) walkFrom(n *node, i int) *walker {
	return &walker{t: t, n: n, i: i}
}

// next advances to the next leaf record and returns its key and data, or ok false at the end.
func (w *walker) next(ctx context.Context) (key, data []byte, ok bool, err error) {
	for {
		w.i++
		if w.i < w.n.numRecords() {
			key, data, err := w.t.key(w.n, w.i)
			if err != nil {
				return nil, nil, false, err
			}
			return key, data, true, nil
		}
		next := w.n.fLink
		if next == 0 {
			return nil, nil, false, nil
		}
		if next >= w.t.totalNodes {
			return nil, nil, false, corruptNode(w.t.name, w.n.num, "fLink %d beyond the tree's %d nodes", next, w.t.totalNodes)
		}
		if w.visited == nil {
			w.visited = make([]uint64, (w.t.totalNodes+63)/64)
			w.visited[w.n.num/64] |= 1 << (w.n.num % 64)
		}
		if w.visited[next/64]&(1<<(next%64)) != 0 {
			return nil, nil, false, corruptNode(w.t.name, next, "leaf chain loops back to this node")
		}
		w.visited[next/64] |= 1 << (next % 64)
		n, err := w.t.node(ctx, next)
		if err != nil {
			return nil, nil, false, err
		}
		if n.kind != kindLeaf {
			return nil, nil, false, corruptNode(w.t.name, next, "leaf chain reaches a node of kind %d", n.kind)
		}
		if n.bLink != w.n.num {
			return nil, nil, false, corruptNode(w.t.name, next, "bLink %d, reached from node %d", n.bLink, w.n.num)
		}
		w.n, w.i = n, -1
	}
}

// nodeCache is an LRU cache of validated nodes, bounded in bytes.
type nodeCache struct {
	mu    sync.Mutex
	limit int
	used  int
	lru   *list.List
	byKey map[cacheKey]*list.Element
}

type cacheKey struct {
	tree int
	num  uint32
}

type cacheEntry struct {
	key  cacheKey
	n    *node
	size int
}

func newNodeCache(limit int) *nodeCache {
	return &nodeCache{limit: limit, lru: list.New(), byKey: make(map[cacheKey]*list.Element)}
}

func (c *nodeCache) get(tree int, num uint32) *node {
	if c.limit <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.byKey[cacheKey{tree, num}]; ok {
		c.lru.MoveToFront(e)
		return e.Value.(*cacheEntry).n
	}
	return nil
}

func (c *nodeCache) put(tree int, num uint32, n *node, size int) {
	if c.limit <= 0 || size > c.limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := cacheKey{tree, num}
	if _, ok := c.byKey[k]; ok {
		return
	}
	c.byKey[k] = c.lru.PushFront(&cacheEntry{key: k, n: n, size: size})
	c.used += size
	for c.used > c.limit {
		e := c.lru.Back()
		ce := e.Value.(*cacheEntry)
		c.lru.Remove(e)
		delete(c.byKey, ce.key)
		c.used -= ce.size
	}
}
