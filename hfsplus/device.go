package hfsplus

import (
	"fmt"
	"io"
	"slices"
)

// device is the partition the volume lives on, read through the journal overlay when there is
// one. Every read is checked against the partition's size, so a structure that points past the end
// is an error rather than a short read.
type device struct {
	r    io.ReaderAt
	size int64

	// overlay maps a journal block number, a block of ov.unit bytes of the partition, to where
	// the newest copy of that block sits in the journal. Nil when nothing is replayed.
	overlay map[int64]int64
	unit    int64
	jnl     *journal
}

func (d *device) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off > d.size {
		return 0, fmt.Errorf("hfsplus: read at %d outside the %d-byte partition: %w", off, d.size, io.EOF)
	}
	var err error
	if int64(len(p)) > d.size-off {
		p = p[:d.size-off]
		err = io.EOF
	}
	if d.overlay == nil {
		n, rerr := d.r.ReadAt(p, off)
		if n == len(p) {
			return n, err
		}
		if rerr == nil {
			rerr = io.ErrUnexpectedEOF
		}
		return n, rerr
	}
	done := 0
	for done < len(p) {
		pos := off + int64(done)
		blk := pos / d.unit
		in := pos - blk*d.unit
		n := int(min(d.unit-in, int64(len(p)-done)))
		if jpos, ok := d.overlay[blk]; ok {
			if rerr := d.jnl.read(p[done:done+n], jpos+in); rerr != nil {
				return done, rerr
			}
			done += n
			continue
		}
		// Extend the plain read over every following block the overlay does not hold.
		for done+n < len(p) {
			if _, ok := d.overlay[(pos+int64(n))/d.unit]; ok {
				break
			}
			n = int(min(int64(n)+d.unit, int64(len(p)-done)))
		}
		m, rerr := d.r.ReadAt(p[done:done+n], pos)
		done += m
		if m < n {
			if rerr == nil {
				rerr = io.ErrUnexpectedEOF
			}
			return done, rerr
		}
	}
	return done, err
}

// readFull reads exactly len(p) bytes at off or returns an error naming the structure.
func (d *device) readFull(p []byte, off int64, what string) error {
	if off < 0 || off > d.size || int64(len(p)) > d.size-off {
		return corrupt(what, off, "%d bytes at %d extend past the %d-byte partition", len(p), off, d.size)
	}
	if _, err := d.ReadAt(p, off); err != nil {
		return fmt.Errorf("hfsplus: reading %s: %w", what, err)
	}
	return nil
}

// overlayBlocks returns the partition blocks the overlay replaces, sorted, for tests and reports.
func (d *device) overlayBlocks() []int64 {
	bs := make([]int64, 0, len(d.overlay))
	for b := range d.overlay {
		bs = append(bs, b)
	}
	slices.Sort(bs)
	return bs
}
