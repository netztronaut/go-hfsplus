package hfsplus

import (
	"encoding/binary"
	"fmt"
	"io"
)

// The HFS+ journal, as xnu's vfs_journal.c writes it: a journal info block in the volume points at
// a circular buffer that starts with a journal header. Transactions are runs of block list headers,
// each followed by the blocks it lists; replaying them copies each block to its home location.
// The journal is written in the writer's byte order, which the header's endian field records.

const (
	jibInFS       = 0x1 // kJIJournalInFSMask
	jibOtherDev   = 0x2 // kJIJournalOnOtherDeviceMask
	jibNeedInit   = 0x4 // kJIJournalNeedInitMask
	jibSize       = 512
	journalMagic  = 0x4a4e4c78 // "JNLx"
	journalEndian = 0x12345678

	journalHeaderChecksumSize = 44 // offsetof(journal_header, sequence_num)
	blhdrChecksumSize         = 32 // the fixed header and binfo[0]
	blhdrCheckChecksums       = 0x1
	blhdrFirstHeader          = 0x2
	blockInfoSize             = 16

	// maxJournalBlockSize bounds jhdr_size and the size of one journaled block.
	maxJournalBlockSize = 1 << 20
)

// JournalInfo reports the state of the volume's journal.
type JournalInfo struct {
	// Journaled is the volume header's journaled attribute; the rest is zero when it is false.
	Journaled bool
	// CleanlyUnmounted is the volume header's unmounted attribute. A volume the guest was killed
	// with has it clear, journaled or not.
	CleanlyUnmounted bool

	// InFilesystem and External are the journal info block's flags. An external journal is not
	// supported; Open reports it with Pending false and Replayed false.
	InFilesystem bool
	External     bool
	NeedsInit    bool

	// Offset and Size locate the journal on the partition, in bytes.
	Offset int64
	Size   int64

	// The journal header.
	BigEndian   bool   // the writer's byte order: true for a PowerPC, false for an Intel Mac
	Start, End  int64  // the first unreplayed transaction and the end of the last one
	BlockSize   int64  // jhdr_size, which is also the unit of the block numbers it records
	Sequence    uint32 // sequence_num, 0 on journals older than Mac OS X 10.5
	HeaderValid bool   // the header's magic, endian and checksum were right

	// Pending reports that the journal holds transactions: Start != End, or valid transactions
	// written past End that xnu would also replay.
	Pending bool
	// Replayed reports that the pending transactions are applied, in memory, to every read.
	Replayed bool
	// Transactions and Blocks count the block list headers and blocks replayed; BlocksKilled
	// counts the blocks the journal recorded and then cancelled.
	Transactions int
	Blocks       int
	BlocksKilled int
	// PastEnd counts block list headers found beyond End and replayed, as xnu does when the last
	// journal header update did not reach the disk.
	PastEnd int

	// Err is why the journal could not be read, with Options.Journal set to OnDisk, which opens
	// the volume anyway; with the other modes Open returns the error instead.
	Err error
}

// journal reads the circular journal buffer.
type journal struct {
	r        io.ReaderAt // the partition, without any overlay
	partSize int64
	off      int64 // partition offset of the journal
	size     int64 // journal size, header included
	hdrSize  int64 // jhdr_size: the header occupies [0, hdrSize)
	order    binary.ByteOrder
}

// read reads len(p) bytes at journal offset pos, wrapping from the end of the buffer to hdrSize.
func (j *journal) read(p []byte, pos int64) error {
	for len(p) > 0 {
		pos = j.wrap(pos)
		n := min(int64(len(p)), j.size-pos)
		off := j.off + pos
		if off+n > j.partSize {
			return corrupt("journal", off, "journal extends past the partition")
		}
		m, err := j.r.ReadAt(p[:n], off)
		if int64(m) < n {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("hfsplus: reading journal: %w", err)
		}
		p = p[n:]
		pos += n
	}
	return nil
}

func (j *journal) wrap(pos int64) int64 {
	if pos >= j.size {
		pos = j.hdrSize + (pos-j.size)%(j.size-j.hdrSize)
	}
	return pos
}

// distance is how far forward end lies from start in the circular buffer.
func (j *journal) distance(start, end int64) int64 {
	if end >= start {
		return end - start
	}
	return (j.size - start) + (end - j.hdrSize)
}

// journalChecksum is xnu's calc_checksum.
func journalChecksum(b []byte) uint32 {
	var c uint32
	for _, x := range b {
		c = (c << 8) ^ (c + uint32(x))
	}
	return ^c
}

// openJournal reads the journal info block and the journal header, and, when replay is wanted,
// the pending transactions into an overlay on dev. volOff is the partition offset of the HFS+
// volume, which is not 0 inside an HFS wrapper.
func openJournal(dev *device, vh *VolumeHeader, volOff int64, replay bool, maxBlocks int) (*JournalInfo, error) {
	info := &JournalInfo{
		Journaled:        vh.Attributes&VolumeJournaled != 0,
		CleanlyUnmounted: vh.Attributes&VolumeUnmounted != 0,
	}
	if !info.Journaled {
		return info, nil
	}
	jibOff := volOff + int64(vh.JournalInfoBlock)*int64(vh.BlockSize)
	var jib [jibSize]byte
	if err := dev.readFull(jib[:], jibOff, "journal info block"); err != nil {
		return nil, err
	}
	flags := be32(jib[0:])
	info.InFilesystem = flags&jibInFS != 0
	info.External = flags&jibOtherDev != 0
	info.NeedsInit = flags&jibNeedInit != 0
	if info.External || !info.InFilesystem {
		return info, nil
	}
	info.Offset = volOff + int64(be64(jib[36:]))
	info.Size = int64(be64(jib[44:]))
	if info.Offset < 0 || info.Size < 2*headerSize || info.Offset > dev.size || info.Size > dev.size-info.Offset {
		return nil, corrupt("journal info block", jibOff, "journal at %d, %d bytes, outside the partition", info.Offset, info.Size)
	}
	if info.NeedsInit {
		// The journal was never initialised, so it holds nothing to replay.
		return info, nil
	}

	var hdr [journalHeaderChecksumSize + 4]byte
	if err := dev.readFull(hdr[:], info.Offset, "journal header"); err != nil {
		return nil, err
	}
	var order binary.ByteOrder
	switch {
	case binary.BigEndian.Uint32(hdr[0:]) == journalMagic:
		order = binary.BigEndian
	case binary.LittleEndian.Uint32(hdr[0:]) == journalMagic:
		order = binary.LittleEndian
	default:
		return nil, corrupt("journal header", info.Offset, "magic %#08x", binary.BigEndian.Uint32(hdr[0:]))
	}
	info.BigEndian = order == binary.BigEndian
	if e := order.Uint32(hdr[4:]); e != journalEndian {
		return nil, corrupt("journal header", info.Offset, "endian field %#08x", e)
	}
	info.Start = int64(order.Uint64(hdr[8:]))
	info.End = int64(order.Uint64(hdr[16:]))
	size := int64(order.Uint64(hdr[24:]))
	blhdrSize := int64(order.Uint32(hdr[32:]))
	sum := order.Uint32(hdr[36:])
	info.BlockSize = int64(order.Uint32(hdr[40:]))
	info.Sequence = order.Uint32(hdr[44:])

	check := hdr
	clear(check[36:40])
	if journalChecksum(check[:journalHeaderChecksumSize]) != sum {
		return nil, corrupt("journal header", info.Offset, "checksum %#08x, computed %#08x", sum, journalChecksum(check[:journalHeaderChecksumSize]))
	}
	switch {
	case info.BlockSize < 512 || info.BlockSize > maxJournalBlockSize || info.BlockSize&(info.BlockSize-1) != 0:
		return nil, corrupt("journal header", info.Offset, "jhdr_size %d", info.BlockSize)
	case size != info.Size:
		return nil, corrupt("journal header", info.Offset, "size %d, journal info block says %d", size, info.Size)
	case blhdrSize < blhdrChecksumSize || blhdrSize > size/2 || blhdrSize%info.BlockSize != 0:
		return nil, corrupt("journal header", info.Offset, "blhdr_size %d", blhdrSize)
	case info.Start < info.BlockSize || info.Start >= size || info.End < info.BlockSize || info.End >= size:
		return nil, corrupt("journal header", info.Offset, "start %d, end %d outside [%d, %d)", info.Start, info.End, info.BlockSize, size)
	}
	info.HeaderValid = true

	j := &journal{r: dev.r, partSize: dev.size, off: info.Offset, size: size, hdrSize: info.BlockSize, order: order}
	ov, err := j.scan(info, blhdrSize, maxBlocks, replay)
	if err != nil {
		return nil, err
	}
	if replay && len(ov) > 0 {
		dev.overlay = ov
		dev.unit = info.BlockSize
		dev.jnl = j
		info.Replayed = true
	}
	return info, nil
}

// scan walks the transactions from start to end, recording in an overlay where the newest copy
// of every journaled block is; with record false it only counts. Like xnu's replay_journal it then
// goes on past end while it finds transactions that continue the sequence with valid checksums:
// what a crash leaves when a transaction reached the journal but the header update after it did
// not.
func (j *journal) scan(info *JournalInfo, blhdrSize int64, maxBlocks int, record bool) (map[int64]int64, error) {
	if info.Start == info.End {
		return nil, nil
	}
	var ov map[int64]int64
	if record {
		ov = make(map[int64]int64)
	}
	buf := make([]byte, blhdrSize)
	var block []byte
	pos := info.Start
	pastEnd := false
	var lastSeq uint32
	var consumed int64
	for {
		if pos == info.End {
			pastEnd = true
		}
		if consumed >= j.size-j.hdrSize {
			if pastEnd {
				break
			}
			return nil, corrupt("journal", j.off+pos, "transactions from start %d never reach end %d", info.Start, info.End)
		}
		if err := j.read(buf, pos); err != nil {
			return nil, err
		}
		bh, err := j.decodeBlockList(buf, pos, blhdrSize)
		if pastEnd {
			// Beyond end only a header whose checksums were written and that continues the
			// sequence counts; anything else is an older transaction the buffer still holds.
			if err != nil || bh.flags&blhdrCheckChecksums == 0 || lastSeq == 0 ||
				bh.seq != lastSeq && bh.seq != lastSeq+1 || consumed+bh.bytesUsed > j.size-j.hdrSize {
				break
			}
		} else if err != nil {
			return nil, err
		}
		if !pastEnd && lastSeq != 0 && bh.seq != 0 && bh.seq < lastSeq {
			return nil, corrupt("journal", j.off+pos, "sequence number %d after %d", bh.seq, lastSeq)
		}

		// Check the blocks' checksums before recording any of them, so that a transaction past
		// end is taken whole or not at all.
		if bh.flags&blhdrCheckChecksums != 0 {
			data := pos + blhdrSize
			bad := false
			for i := 1; i < bh.numBlocks && !bad; i++ {
				bi := buf[blockInfoSize*(i+1):]
				bsize := int64(j.order.Uint32(bi[8:]))
				if int64(cap(block)) < bsize {
					block = make([]byte, bsize)
				}
				if err := j.read(block[:bsize], data); err != nil {
					return nil, err
				}
				if c := journalChecksum(block[:bsize]); c != j.order.Uint32(bi[12:]) {
					if !pastEnd {
						return nil, corrupt("journal", j.off+data, "block %d of the transaction at %d: checksum %#08x, computed %#08x", i, pos, j.order.Uint32(bi[12:]), c)
					}
					bad = true
				}
				data += bsize
			}
			if bad {
				break
			}
		}

		data := pos + blhdrSize
		for i := 1; i < bh.numBlocks; i++ {
			bi := buf[blockInfoSize*(i+1):]
			bnum := j.order.Uint64(bi[0:])
			bsize := int64(j.order.Uint32(bi[8:]))
			switch {
			case bnum == ^uint64(0):
				info.BlocksKilled++
			case bsize%j.hdrSize != 0 || bnum > uint64(j.partSize/j.hdrSize) || int64(bnum)*j.hdrSize+bsize > j.partSize:
				return nil, corrupt("journal", j.off+pos, "block %d: %d bytes at block %d, outside the partition", i, bsize, bnum)
			case record:
				for k := int64(0); k < bsize/j.hdrSize; k++ {
					ov[int64(bnum)+k] = j.wrap(data + k*j.hdrSize)
				}
				if maxBlocks > 0 && len(ov) > maxBlocks {
					return nil, unsupported("journal replay of more than %d blocks (Options.MaxJournalBlocks)", maxBlocks)
				}
			}
			info.Blocks++
			data += bsize
		}
		info.Transactions++
		if pastEnd {
			info.PastEnd++
		}
		if bh.seq != 0 {
			lastSeq = bh.seq
		}
		consumed += bh.bytesUsed
		pos = j.wrap(pos + bh.bytesUsed)
	}
	info.Pending = info.Transactions > 0
	return ov, nil
}

type blockList struct {
	maxBlocks int
	numBlocks int // block infos, the first of which describes the list itself
	bytesUsed int64
	flags     uint32
	seq       uint32
}

// decodeBlockList checks a block list header read at journal offset pos. Its fixed part is
// max_blocks, num_blocks, bytes_used, checksum and flags; block_info entries of 16 bytes follow,
// the first holding the transaction's sequence number, the others a block number (in units of
// jhdr_size), a size and a checksum each.
func (j *journal) decodeBlockList(b []byte, pos, blhdrSize int64) (blockList, error) {
	o := j.order
	bh := blockList{
		maxBlocks: int(o.Uint16(b[0:])),
		numBlocks: int(o.Uint16(b[2:])),
		bytesUsed: int64(int32(o.Uint32(b[4:]))),
		flags:     o.Uint32(b[12:]),
		seq:       o.Uint32(b[16+12:]),
	}
	sum := o.Uint32(b[8:])
	fail := func(format string, args ...any) (blockList, error) {
		return bh, corrupt("journal", j.off+pos, "block list header: "+format, args...)
	}
	if bh.flags&blhdrCheckChecksums != 0 {
		var head [blhdrChecksumSize]byte
		copy(head[:], b)
		clear(head[8:12])
		if c := journalChecksum(head[:]); c != sum {
			return fail("checksum %#08x, computed %#08x", sum, c)
		}
	}
	switch {
	case bh.numBlocks < 1 || int64(bh.numBlocks+1)*blockInfoSize > blhdrSize:
		return fail("%d blocks in a %d-byte header", bh.numBlocks, blhdrSize)
	case bh.maxBlocks != 0 && bh.numBlocks > bh.maxBlocks:
		return fail("%d blocks, at most %d", bh.numBlocks, bh.maxBlocks)
	case bh.bytesUsed < blhdrSize || bh.bytesUsed > j.size-j.hdrSize:
		return fail("bytes_used %d", bh.bytesUsed)
	}
	var total int64
	for i := 1; i < bh.numBlocks; i++ {
		bsize := int64(o.Uint32(b[blockInfoSize*(i+1)+8:]))
		if bsize <= 0 || bsize > maxJournalBlockSize {
			return fail("block %d is %d bytes", i, bsize)
		}
		total += bsize
	}
	if blhdrSize+total != bh.bytesUsed {
		return fail("blocks take %d bytes, bytes_used is %d", blhdrSize+total, bh.bytesUsed)
	}
	return bh, nil
}
