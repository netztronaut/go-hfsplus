package hfsplus

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"netztronaut.de/go-hfsplus/lzfse"
	"netztronaut.de/go-hfsplus/lzvn"
)

// decmpfs, the transparent compression of Mac OS X 10.6 and later: a file flagged UF_COMPRESSED
// has an empty data fork and a com.apple.decmpfs attribute whose little-endian header gives the
// compression type and the uncompressed size. Small files keep their data after the header;
// larger ones keep it in the resource fork, in 64 KiB chunks.

const (
	decmpfsMagic      = 0x636d7066 // "fpmc" read little-endian
	decmpfsHeaderSize = 16
	decmpfsChunkSize  = 64 << 10

	// maxInlineSize bounds the uncompressed size of an attribute-held file, which is decoded
	// whole; macOS only keeps small files that way.
	maxInlineSize = 64 << 20
	// maxChunkSize bounds one compressed chunk: a chunk that did not compress is stored with a
	// one-byte marker, and no algorithm expands a 64 KiB chunk by more than a few hundred bytes.
	maxChunkSize = 2 * decmpfsChunkSize
	// maxDecmpfsAttr bounds the com.apple.decmpfs attribute read.
	maxDecmpfsAttr = 1 << 20
)

// Decompressor decodes one decmpfs chunk, or the data of an attribute-held file, from src into
// dst, whose length is the chunk's uncompressed size, and returns the bytes written. It must not
// retain src or dst. Stored-uncompressed chunks never reach it.
type Decompressor func(dst, src []byte) (int, error)

func decompressZlib(dst, src []byte) (int, error) {
	zr, err := zlib.NewReader(bytes.NewReader(src))
	if err != nil {
		return 0, err
	}
	n, err := io.ReadFull(zr, dst)
	if err == io.ErrUnexpectedEOF {
		err = nil
	}
	return n, err
}

func decompressLZVN(dst, src []byte) (int, error)  { return lzvn.Decode(dst, src) }
func decompressLZFSE(dst, src []byte) (int, error) { return lzfse.Decode(dst, src) }

// DecmpfsHeader is the decoded header of a com.apple.decmpfs attribute.
type DecmpfsHeader struct {
	Type             uint32
	UncompressedSize uint64
}

func decodeDecmpfs(b []byte) (DecmpfsHeader, error) {
	if len(b) < decmpfsHeaderSize {
		return DecmpfsHeader{}, corrupt("decmpfs", -1, "attribute is %d bytes", len(b))
	}
	if m := binary.LittleEndian.Uint32(b); m != decmpfsMagic {
		return DecmpfsHeader{}, corrupt("decmpfs", -1, "magic %#08x", m)
	}
	return DecmpfsHeader{Type: binary.LittleEndian.Uint32(b[4:]), UncompressedSize: binary.LittleEndian.Uint64(b[8:])}, nil
}

// Layouts of the compression types.
func decmpfsInline(t uint32) bool { return t == 1 || t == 3 || t == 7 || t == 11 || t == 13 }
func decmpfsInRsrc(t uint32) bool { return t == 4 || t == 8 || t == 12 || t == 14 }

// isRawChunk reports whether a chunk of compression type t is stored uncompressed, which each
// algorithm marks with a first byte its streams cannot start with.
func isRawChunk(t uint32, b []byte) bool {
	switch t {
	case 3, 4:
		return b[0]&0x0F == 0x0F // a zlib stream's first byte names deflate, 8
	case 7, 8:
		return b[0] == 0x06 // LZVN's end-of-stream opcode
	default:
		return b[0] == 0xFF
	}
}

// decmpfsFile reads a compressed file's uncompressed bytes.
type decmpfsFile struct {
	v    *volume
	cnid uint32
	hdr  DecmpfsHeader
	size int64
	dec  Decompressor

	data []byte // an attribute-held file, decoded

	rsrc    io.ReaderAt // the resource fork, for chunked types
	rsrcLen int64
	nchunks int64
	tabOff  int64 // type 4: where the chunk table's entries start; its offsets are relative to base
	base    int64

	mu      sync.Mutex
	cached  int64 // index of the chunk in buf, -1 for none
	buf     []byte
	scratch []byte
}

// openDecmpfs prepares reading the compressed file rec, whose com.apple.decmpfs attribute is a.
func (v *volume) openDecmpfs(ctx context.Context, rec *Record, a *attribute) (*decmpfsFile, error) {
	raw, err := v.readAttr(ctx, rec.CNID, a, maxDecmpfsAttr)
	if err != nil {
		return nil, err
	}
	h, err := decodeDecmpfs(raw)
	if err != nil {
		return nil, fmt.Errorf("CNID %d: %w", rec.CNID, err)
	}
	if h.UncompressedSize > 1<<62 {
		return nil, corrupt("decmpfs", -1, "CNID %d: uncompressed size %d", rec.CNID, h.UncompressedSize)
	}
	d := &decmpfsFile{v: v, cnid: rec.CNID, hdr: h, size: int64(h.UncompressedSize), cached: -1}
	if h.Type != 1 {
		d.dec = v.decompressors[h.Type]
		if d.dec == nil || !(decmpfsInline(h.Type) || decmpfsInRsrc(h.Type)) {
			return nil, unsupported("decmpfs compression type %d (CNID %d)", h.Type, rec.CNID)
		}
	}
	switch {
	case decmpfsInline(h.Type):
		if d.size > maxInlineSize {
			return nil, unsupported("decmpfs attribute-held file of %d bytes (CNID %d)", d.size, rec.CNID)
		}
		src := raw[decmpfsHeaderSize:]
		d.data = make([]byte, d.size)
		if d.size == 0 {
			return d, nil
		}
		if len(src) == 0 {
			return nil, corrupt("decmpfs", -1, "CNID %d: no data after the header", rec.CNID)
		}
		if h.Type == 1 {
			if len(src) < len(d.data) {
				return nil, corrupt("decmpfs", -1, "CNID %d: %d bytes stored for %d", rec.CNID, len(src), d.size)
			}
			copy(d.data, src)
			return d, nil
		}
		if err := d.decode(d.data, src); err != nil {
			return nil, err
		}
		return d, nil
	case decmpfsInRsrc(h.Type):
		f, err := v.newFork(fmt.Sprintf("resource fork of CNID %d", rec.CNID), rec.ResourceFork, v.overflow(rec.CNID, forkResource))
		if err != nil {
			return nil, err
		}
		d.rsrc = forkReader{ctx: ctx, f: f}
		d.rsrcLen = f.size
		d.nchunks = (d.size + decmpfsChunkSize - 1) / decmpfsChunkSize
		if err := d.readTable(); err != nil {
			return nil, err
		}
		return d, nil
	}
	return nil, unsupported("decmpfs compression type %d (CNID %d)", h.Type, rec.CNID)
}

func (d *decmpfsFile) fail(format string, args ...any) error {
	return corrupt("decmpfs", -1, "CNID %d: "+format, append([]any{d.cnid}, args...)...)
}

// readTable checks the chunk table's header against the uncompressed size.
func (d *decmpfsFile) readTable() error {
	var b [16]byte
	if d.hdr.Type == 4 {
		// A resource fork with one 'cmpf' resource: the fork header gives the data offset; the
		// resource's length, then the little-endian chunk table, follow it.
		if err := d.readRsrc(b[:], 0); err != nil {
			return err
		}
		dataOff := int64(be32(b[0:]))
		if err := d.readRsrc(b[:8], dataOff); err != nil {
			return err
		}
		d.base = dataOff + 4
		n := int64(binary.LittleEndian.Uint32(b[4:]))
		if n != d.nchunks {
			return d.fail("%d chunks for %d bytes", n, d.size)
		}
		d.tabOff = d.base + 4
		if d.tabOff+8*n > d.rsrcLen {
			return d.fail("chunk table past the resource fork")
		}
		return nil
	}
	// Types 8, 12, 14: the fork starts with nchunks+1 little-endian offsets.
	if err := d.readRsrc(b[:4], 0); err != nil {
		return err
	}
	if first := int64(binary.LittleEndian.Uint32(b[:])); first != 4*(d.nchunks+1) {
		return d.fail("chunk table of %d bytes for %d chunks", first, d.nchunks)
	}
	return nil
}

func (d *decmpfsFile) readRsrc(p []byte, off int64) error {
	if off < 0 || off+int64(len(p)) > d.rsrcLen {
		return d.fail("%d bytes at %d past the %d-byte resource fork", len(p), off, d.rsrcLen)
	}
	if _, err := d.rsrc.ReadAt(p, off); err != nil && err != io.EOF {
		return err
	}
	return nil
}

// chunkRange returns where chunk i's compressed bytes are in the resource fork.
func (d *decmpfsFile) chunkRange(i int64) (int64, int64, error) {
	// Type 4 entries are (offset, size) pairs; the others read offsets i and i+1.
	entry := d.tabOff + 4*i
	if d.hdr.Type == 4 {
		entry = d.tabOff + 8*i
	}
	var b [8]byte
	if err := d.readRsrc(b[:], entry); err != nil {
		return 0, 0, err
	}
	var off, n int64
	if d.hdr.Type == 4 {
		off = d.base + int64(binary.LittleEndian.Uint32(b[0:]))
		n = int64(binary.LittleEndian.Uint32(b[4:]))
	} else {
		start, end := int64(binary.LittleEndian.Uint32(b[0:])), int64(binary.LittleEndian.Uint32(b[4:]))
		if end < start {
			return 0, 0, d.fail("chunk %d ends at %d before it starts at %d", i, end, start)
		}
		off, n = start, end-start
	}
	if n == 0 || n > maxChunkSize || off+n > d.rsrcLen {
		return 0, 0, d.fail("chunk %d: %d bytes at %d", i, n, off)
	}
	return off, n, nil
}

func (d *decmpfsFile) decode(dst, src []byte) error {
	if len(src) == 0 {
		return d.fail("empty chunk")
	}
	if isRawChunk(d.hdr.Type, src) {
		if len(src)-1 < len(dst) {
			return d.fail("stored chunk of %d bytes for %d", len(src)-1, len(dst))
		}
		copy(dst, src[1:])
		return nil
	}
	n, err := d.dec(dst, src)
	if err != nil {
		return d.fail("decompressing (type %d): %v", d.hdr.Type, err)
	}
	if n != len(dst) {
		return d.fail("chunk decompressed to %d bytes, expected %d", n, len(dst))
	}
	return nil
}

// ReadAt reads the uncompressed file.
func (d *decmpfsFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errInvalidOffset
	}
	if off >= d.size {
		return 0, io.EOF
	}
	if d.data != nil || d.size == 0 {
		return bytesReaderAt(d.data).ReadAt(p, off)
	}
	var eof error
	if int64(len(p)) > d.size-off {
		p, eof = p[:d.size-off], io.EOF
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	done := 0
	for done < len(p) {
		pos := off + int64(done)
		i := pos / decmpfsChunkSize
		if d.cached != i {
			if err := d.load(i); err != nil {
				return done, err
			}
		}
		done += copy(p[done:], d.buf[pos-i*decmpfsChunkSize:])
	}
	return done, eof
}

func (d *decmpfsFile) load(i int64) error {
	off, n, err := d.chunkRange(i)
	if err != nil {
		return err
	}
	if cap(d.scratch) < int(n) {
		d.scratch = make([]byte, n)
	}
	src := d.scratch[:n]
	if err := d.readRsrc(src, off); err != nil {
		return err
	}
	want := min(decmpfsChunkSize, d.size-i*decmpfsChunkSize)
	if d.buf == nil {
		d.buf = make([]byte, decmpfsChunkSize)
	}
	d.cached = -1
	if err := d.decode(d.buf[:want], src); err != nil {
		return fmt.Errorf("chunk %d: %w", i, err)
	}
	d.buf = d.buf[:want]
	d.cached = i
	return nil
}
