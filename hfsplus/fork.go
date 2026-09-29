package hfsplus

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
)

// Fork types in an extents overflow key.
const (
	forkData     = 0x00
	forkResource = 0xFF
)

// maxExtentRecords bounds the extent records one fork may use, beyond which the chain is taken to
// be corrupt: a fork of 2^32 blocks in one-block extents would need 2^29 records.
const maxExtentRecords = 1 << 29

// fork reads the bytes of one fork: its first eight extents from the catalog (or the volume
// header, or an attribute record), the rest looked up on demand, eight at a time.
type fork struct {
	v         *volume
	what      string // for errors: "catalog file", "data fork of CNID 1234", ...
	size      int64
	total     uint32 // allocation blocks the fork claims
	more      func(ctx context.Context, startBlock uint32) (*[8]Extent, error)
	mu        sync.Mutex
	extents   []Extent // extents found so far, in file order
	starts    []uint32 // the fork block each of extents starts at
	covered   uint32   // allocation blocks the found extents cover
	exhausted bool     // no more records will be found
	records   int
}

// newFork returns a fork over fd. more, when not nil, finds the extent record that starts at a
// given file block; it is the extents overflow B-tree for catalog forks and the attributes B-tree
// for attribute forks.
func (v *volume) newFork(what string, fd ForkData, more func(ctx context.Context, startBlock uint32) (*[8]Extent, error)) (*fork, error) {
	f := &fork{v: v, what: what, total: fd.TotalBlocks, more: more}
	if fd.LogicalSize > uint64(fd.TotalBlocks)*uint64(v.blockSize) {
		return nil, corrupt(what, -1, "logical size %d exceeds %d blocks of %d bytes", fd.LogicalSize, fd.TotalBlocks, v.blockSize)
	}
	f.size = int64(fd.LogicalSize)
	if err := f.add(&fd.Extents); err != nil {
		return nil, err
	}
	return f, nil
}

// add appends an extent record, stopping at its first empty extent.
func (f *fork) add(rec *[8]Extent) error {
	for _, e := range rec {
		if e.BlockCount == 0 {
			// An unused slot ends the fork's extents: no overflow record follows it.
			f.exhausted = true
			break
		}
		if uint64(e.StartBlock)+uint64(e.BlockCount) > uint64(f.v.totalBlocks) {
			return corrupt(f.what, -1, "extent %d+%d past the volume's %d blocks", e.StartBlock, e.BlockCount, f.v.totalBlocks)
		}
		if uint64(f.covered)+uint64(e.BlockCount) > uint64(f.total) {
			return corrupt(f.what, -1, "extents cover more than the fork's %d blocks", f.total)
		}
		f.extents = append(f.extents, e)
		f.starts = append(f.starts, f.covered)
		f.covered += e.BlockCount
	}
	if f.covered >= f.total {
		f.exhausted = true
	}
	return nil
}

// locate returns the volume byte offset of the fork's byte off, and how many bytes from there are
// contiguous on the volume.
func (f *fork) locate(ctx context.Context, off int64) (int64, int64, error) {
	bs := int64(f.v.blockSize)
	blk := off / bs
	f.mu.Lock()
	defer f.mu.Unlock()
	for uint64(blk) >= uint64(f.covered) {
		if f.exhausted || f.more == nil {
			return 0, 0, corrupt(f.what, -1, "byte %d is beyond the %d blocks its extents cover", off, f.covered)
		}
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		f.records++
		if f.records > maxExtentRecords {
			return 0, 0, corrupt(f.what, -1, "more than %d extent records", maxExtentRecords)
		}
		rec, err := f.more(ctx, f.covered)
		if err != nil {
			return 0, 0, err
		}
		if rec == nil {
			return 0, 0, corrupt(f.what, -1, "no extent record for block %d of %d", f.covered, f.total)
		}
		before := f.covered
		if err := f.add(rec); err != nil {
			return 0, 0, err
		}
		if f.covered == before {
			return 0, 0, corrupt(f.what, -1, "empty extent record for block %d", before)
		}
	}
	i, found := slices.BinarySearch(f.starts, uint32(blk))
	if !found {
		i--
	}
	e := f.extents[i]
	in := blk - int64(f.starts[i])
	volOff := (int64(e.StartBlock)+in)*bs + off%bs
	run := (int64(e.BlockCount)-in)*bs - off%bs
	return volOff, run, nil
}

// readAt reads len(p) bytes of the fork at off. It reads fewer, with io.EOF, only at the end of
// the fork's logical size.
func (f *fork) readAt(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("hfsplus: negative offset: %w", errInvalidOffset)
	}
	if off >= f.size {
		return 0, io.EOF
	}
	var eof error
	if int64(len(p)) > f.size-off {
		p = p[:f.size-off]
		eof = io.EOF
	}
	done := 0
	for done < len(p) {
		if done > 0 {
			if err := ctx.Err(); err != nil {
				return done, err
			}
		}
		volOff, run, err := f.locate(ctx, off+int64(done))
		if err != nil {
			return done, err
		}
		n := int(min(run, int64(len(p)-done)))
		if err := f.v.dev.readFull(p[done:done+n], f.v.offset+volOff, f.what); err != nil {
			return done, err
		}
		done += n
	}
	return done, eof
}

// forkReader adapts a fork to io.ReaderAt with a fixed context.
type forkReader struct {
	ctx context.Context
	f   *fork
}

func (r forkReader) ReadAt(p []byte, off int64) (int, error) { return r.f.readAt(r.ctx, p, off) }
