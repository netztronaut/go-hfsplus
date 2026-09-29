// Package apm reads an Apple Partition Map, the partitioning scheme of PowerPC Macs and of Apple
// optical media, from an io.ReaderAt. It never writes.
//
// The layout is Inside Macintosh: Devices, "SCSI Manager", and Apple's pmap(8): block 0 of the
// device holds a Driver Descriptor Record (signature "ER"), and the partition map itself is a run
// of entries (signature "PM") starting at block 1, each describing one partition, including the
// map itself.
package apm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/netztronaut/go-hfsplus-reader/internal/macroman"
)

// Signatures.
const (
	ddrSignature   = 0x4552 // "ER"
	entrySignature = 0x504D // "PM"
	oldSignature   = 0x5453 // "TS", the pre-1986 map format, which is not supported
)

// SectorSize is the size of a Driver Descriptor Record and of one partition map entry.
const SectorSize = 512

// MaxEntries bounds the number of entries Read accepts, whatever the map claims.
const MaxEntries = 4096

// Partition types Apple defines. Any other type string is passed through unchanged.
const (
	TypePartitionMap = "Apple_partition_map"
	TypeHFS          = "Apple_HFS"
	TypeHFSX         = "Apple_HFSX"
	TypeFree         = "Apple_Free"
	TypeDriver       = "Apple_Driver"
	TypeDriver43     = "Apple_Driver43"
	TypeDriver43CD   = "Apple_Driver43_CD"
	TypeDriverATA    = "Apple_Driver_ATA"
	TypeDriverATAPI  = "Apple_Driver_ATAPI"
	TypeDriverIOKit  = "Apple_Driver_IOKit"
	TypeFWDriver     = "Apple_FWDriver"
	TypePatches      = "Apple_Patches"
	TypeBootstrap    = "Apple_Bootstrap"
	TypeUFS          = "Apple_UFS"
	TypeBoot         = "Apple_Boot"
	TypeVoid         = "Apple_Void"
	TypeScratch      = "Apple_Scratch"
	TypeUnixSVR2     = "Apple_Unix_SVR2"
	TypeRhapsodyUFS  = "Apple_Rhapsody_UFS"
	TypeProDOS       = "Apple_PRODOS"
)

// Errors. Every error Read returns wraps one of these, so errors.Is works on it.
var (
	// ErrNoMap is returned when neither a Driver Descriptor Record nor a partition map entry is
	// where an Apple Partition Map would have them.
	ErrNoMap = errors.New("apm: no Apple Partition Map")
	// ErrCorrupt is returned for a map that is present but inconsistent.
	ErrCorrupt = errors.New("apm: corrupt partition map")
	// ErrOutsideDisk is returned (in an *EntryError) for an entry that extends past the disk.
	ErrOutsideDisk = errors.New("partition extends past the end of the disk")
	// ErrOverlapsMap is returned (in an *EntryError) for an entry, other than the map's own, that
	// overlaps the blocks the map occupies.
	ErrOverlapsMap = errors.New("partition overlaps the partition map")
	// ErrTooManyEntries is returned (in an *EntryError) when the map claims more entries than fit.
	ErrTooManyEntries = errors.New("partition map claims more entries than fit")
	// ErrUnsupported is returned for the pre-1986 "TS" map format.
	ErrUnsupported = errors.New("apm: unsupported partition map format")
)

// EntryError reports a problem with one partition map entry.
type EntryError struct {
	Index int    // 1-based entry number, as pdisk(8) numbers them
	Name  string // the entry's name, if it could be read
	Err   error  // ErrOutsideDisk, ErrOverlapsMap, ErrTooManyEntries or a wrapped ErrCorrupt
}

func (e *EntryError) Error() string {
	return fmt.Sprintf("apm: entry %d (%q): %v", e.Index, e.Name, e.Err)
}

func (e *EntryError) Unwrap() []error { return []error{e.Err, ErrCorrupt} }

// Status is a partition's pmPartStatus word.
type Status uint32

// Status bits.
const (
	StatusValid           Status = 0x00000001
	StatusAllocated       Status = 0x00000002
	StatusInUse           Status = 0x00000004
	StatusBootable        Status = 0x00000008
	StatusReadable        Status = 0x00000010
	StatusWritable        Status = 0x00000020
	StatusBootPIC         Status = 0x00000040
	StatusChainCompatible Status = 0x00000100
	StatusRealDriver      Status = 0x00000200
	StatusChainDriver     Status = 0x00000400
	StatusAutoMount       Status = 0x40000000
	StatusStartup         Status = 0x80000000
)

// Driver is one driver descriptor of the Driver Descriptor Record.
type Driver struct {
	Block uint32 // first block of the driver, in device blocks
	Size  uint16 // driver size, in 512-byte blocks
	Type  uint16 // operating system or processor type
}

// Partition is one entry of the partition map.
type Partition struct {
	Index int    // 1-based entry number
	Name  string // pmPartName, decoded from Mac OS Roman
	Type  string // pmParType, for instance TypeHFS

	StartBlock uint32 // pmPyPartStart, in units of Map.Unit
	BlockCount uint32 // pmPartBlkCnt, in units of Map.Unit
	Start      int64  // StartBlock in bytes from the start of the disk
	Length     int64  // BlockCount in bytes

	Status Status

	// The logical data area, relative to the start of the partition, in units of Map.Unit.
	DataStart uint32 // pmLgDataStart
	DataCount uint32 // pmDataCnt

	// Boot code, for partitions that carry it (drivers, Apple_Bootstrap on New World Macs).
	BootStart    uint32 // pmLgBootStart, relative to the partition, in units of Map.Unit
	BootSize     uint32 // pmBootSize, in bytes
	BootAddr     uint32 // pmBootAddr
	BootEntry    uint32 // pmBootEntry
	BootChecksum uint32 // pmBootCksum
	Processor    string // pmProcessor, for instance "powerpc"

	// Truncated reports an Apple_Free entry that extends past the end of the disk, as the
	// trailing free space of an optical disc image cut short at its last data block does; Length
	// is then clipped to the disk. Any other entry past the end is an ErrOutsideDisk.
	Truncated bool

	Raw [SectorSize]byte // the entry as it is on disk

	disk io.ReaderAt
}

// Section returns the partition's bytes as a reader that starts at 0. The HFS+ reader is given
// this, so it never sees disk offsets.
func (p *Partition) Section() *io.SectionReader {
	return io.NewSectionReader(p.disk, p.Start, p.Length)
}

// Data returns the partition's logical data area (pmLgDataStart and pmDataCnt). For an Apple_HFS
// partition it is normally the whole partition. When the entry leaves pmDataCnt 0, or the data
// area does not fit the partition, Data returns the whole partition.
func (p *Partition) Data(m *Map) *io.SectionReader {
	start := int64(p.DataStart) * m.Unit
	n := int64(p.DataCount) * m.Unit
	if p.DataCount == 0 || start+n > p.Length {
		return p.Section()
	}
	return io.NewSectionReader(p.disk, p.Start+start, n)
}

// Map is a decoded Apple Partition Map.
type Map struct {
	// HasDDR reports whether block 0 held a Driver Descriptor Record. A map without one is still
	// a map.
	HasDDR bool
	// BlockSize and BlockCount are sbBlkSize and sbBlkCount of the Driver Descriptor Record: the
	// device block size, 512 on disks and 2048 on optical media, and the device size in blocks.
	BlockSize  uint16
	BlockCount uint32
	DeviceType uint16
	DeviceID   uint16
	Drivers    []Driver

	// EntrySize is the distance between partition map entries, in bytes; Unit is the size of the
	// blocks StartBlock, BlockCount, DataStart and BootStart count. Both are determined from the
	// data: on a disk both are 512; on media whose DDR says 2048 they are whatever makes the map
	// consistent.
	EntrySize int64
	Unit      int64

	Partitions []Partition

	// DDR is block 0 as it is on disk, when HasDDR.
	DDR [SectorSize]byte
}

// Read decodes the Apple Partition Map of the disk r, which is size bytes long.
func Read(r io.ReaderAt, size int64) (*Map, error) {
	if size < 2*SectorSize {
		return nil, ErrNoMap
	}
	m := &Map{}
	var ddr [SectorSize]byte
	if err := readFull(r, ddr[:], 0); err != nil {
		return nil, err
	}
	if be16(ddr[0:]) == ddrSignature {
		m.HasDDR = true
		m.DDR = ddr
		m.BlockSize = be16(ddr[2:])
		m.BlockCount = be32(ddr[4:])
		m.DeviceType = be16(ddr[8:])
		m.DeviceID = be16(ddr[10:])
		n := int(be16(ddr[16:]))
		// Descriptors are 8 bytes from offset 18; at most 61 fit in the block.
		if n > (SectorSize-18)/8 {
			n = (SectorSize - 18) / 8
		}
		for i := range n {
			d := ddr[18+8*i:]
			m.Drivers = append(m.Drivers, Driver{Block: be32(d), Size: be16(d[4:]), Type: be16(d[6:])})
		}
	}

	stride, err := findStride(r, size, m)
	if err != nil {
		return nil, err
	}
	m.EntrySize = stride

	var first [SectorSize]byte
	if err := readFull(r, first[:], stride); err != nil {
		return nil, err
	}
	count := be32(first[4:])
	if count == 0 {
		return nil, &EntryError{Index: 1, Name: macroman.Decode(first[16:48]), Err: fmt.Errorf("%w: map block count is 0", ErrCorrupt)}
	}
	if count > MaxEntries || int64(count) > size/stride-1 {
		return nil, &EntryError{Index: 1, Name: macroman.Decode(first[16:48]), Err: fmt.Errorf("%w: %d entries", ErrTooManyEntries, count)}
	}

	for i := int64(1); i <= int64(count); i++ {
		var e [SectorSize]byte
		if err := readFull(r, e[:], i*stride); err != nil {
			return nil, err
		}
		p := Partition{Index: int(i), Raw: e, disk: r}
		p.Name = macroman.Decode(e[16:48])
		if be16(e[0:]) != entrySignature {
			return nil, &EntryError{Index: p.Index, Name: p.Name, Err: fmt.Errorf("%w: signature %#04x, want \"PM\"", ErrCorrupt, be16(e[0:]))}
		}
		if c := be32(e[4:]); c != count {
			return nil, &EntryError{Index: p.Index, Name: p.Name, Err: fmt.Errorf("%w: map block count %d, entry 1 says %d", ErrCorrupt, c, count)}
		}
		p.Type = macroman.Decode(e[48:80])
		p.StartBlock = be32(e[8:])
		p.BlockCount = be32(e[12:])
		p.DataStart = be32(e[80:])
		p.DataCount = be32(e[84:])
		p.Status = Status(be32(e[88:]))
		p.BootStart = be32(e[92:])
		p.BootSize = be32(e[96:])
		p.BootAddr = be32(e[100:])
		p.BootEntry = be32(e[108:])
		p.BootChecksum = be32(e[116:])
		p.Processor = macroman.Decode(e[120:136])
		m.Partitions = append(m.Partitions, p)
	}

	m.Unit = findUnit(m, size)
	for i := range m.Partitions {
		p := &m.Partitions[i]
		p.Start = int64(p.StartBlock) * m.Unit
		p.Length = int64(p.BlockCount) * m.Unit
	}
	if err := m.validate(size); err != nil {
		return nil, err
	}
	return m, nil
}

// findStride finds the distance between entries: the DDR's block size when an entry is there,
// otherwise 512. Apple's optical media put the map at 2048-byte strides on some discs and at
// 512-byte strides on others, so the data decides.
func findStride(r io.ReaderAt, size int64, m *Map) (int64, error) {
	var candidates []int64
	if m.HasDDR && m.BlockSize > SectorSize && m.BlockSize%SectorSize == 0 {
		candidates = append(candidates, int64(m.BlockSize))
	}
	candidates = append(candidates, SectorSize)
	var sig [2]byte
	for _, c := range candidates {
		if 2*c > size {
			continue
		}
		if err := readFull(r, sig[:], c); err != nil {
			return 0, err
		}
		switch be16(sig[:]) {
		case entrySignature:
			return c, nil
		case oldSignature:
			return 0, ErrUnsupported
		}
	}
	return 0, ErrNoMap
}

// findUnit decides what pmPyPartStart counts. The map describes itself: its own entry starts at
// the first entry's offset, so the unit that puts it there is the one. When the map has no entry
// for itself, the unit is the stride. If that unit places a partition past the end of the disk
// and the other candidate does not, the other candidate wins.
func findUnit(m *Map, size int64) int64 {
	candidates := []int64{m.EntrySize}
	if m.EntrySize != SectorSize {
		candidates = append(candidates, SectorSize)
	} else if m.HasDDR && m.BlockSize > SectorSize && m.BlockSize%SectorSize == 0 {
		candidates = append(candidates, int64(m.BlockSize))
	}
	fits := func(unit int64) bool {
		for _, p := range m.Partitions {
			if (int64(p.StartBlock)+int64(p.BlockCount))*unit > size {
				return false
			}
		}
		return true
	}
	for _, p := range m.Partitions {
		if p.Type == TypePartitionMap && p.StartBlock != 0 {
			for _, u := range candidates {
				if int64(p.StartBlock)*u == m.EntrySize && fits(u) {
					return u
				}
			}
		}
	}
	for _, u := range candidates {
		if fits(u) {
			return u
		}
	}
	return m.EntrySize
}

func (m *Map) validate(size int64) error {
	mapStart := m.EntrySize
	mapEnd := m.EntrySize * (1 + int64(len(m.Partitions)))
	for i := range m.Partitions {
		p := &m.Partitions[i]
		if p.Type == TypeFree && p.Start+p.Length > size && p.Start <= size {
			p.Length = size - p.Start
			p.Truncated = true
		}
		if p.Start+p.Length > size || p.Start < 0 {
			return &EntryError{Index: p.Index, Name: p.Name, Err: fmt.Errorf("%w: bytes %d-%d, disk is %d", ErrOutsideDisk, p.Start, p.Start+p.Length, size)}
		}
		if p.Type == TypePartitionMap {
			if p.Start > mapStart || p.Start+p.Length < mapEnd {
				return &EntryError{Index: p.Index, Name: p.Name, Err: fmt.Errorf("%w: %d entries do not fit the map's own %d bytes", ErrTooManyEntries, len(m.Partitions), p.Length)}
			}
			continue
		}
		if p.Length > 0 && p.Start < mapEnd && p.Start+p.Length > mapStart {
			return &EntryError{Index: p.Index, Name: p.Name, Err: fmt.Errorf("%w: bytes %d-%d, map is %d-%d", ErrOverlapsMap, p.Start, p.Start+p.Length, mapStart, mapEnd)}
		}
	}
	return nil
}

// Find returns the first partition of the given type, or nil.
func (m *Map) Find(typ string) *Partition {
	for i := range m.Partitions {
		if m.Partitions[i].Type == typ {
			return &m.Partitions[i]
		}
	}
	return nil
}

// IsDriver reports whether a type string names a driver partition (Apple_Driver*).
func IsDriver(typ string) bool {
	return len(typ) >= len(TypeDriver) && typ[:len(TypeDriver)] == TypeDriver || typ == TypeFWDriver
}

func readFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("apm: reading %d bytes at %d: %w", len(p), off, err)
}

func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
