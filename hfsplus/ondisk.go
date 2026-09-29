package hfsplus

import (
	"encoding/binary"
	"time"
)

// On-disk layout, from Apple's Technical Note TN1150, "HFS Plus Volume Format". Everything is
// big-endian except the journal (the writer's byte order) and decmpfs (little-endian).

const (
	sigHFSPlus = 0x482B // "H+"
	sigHFSX    = 0x4858 // "HX"
	sigHFS     = 0x4244 // "BD", an HFS Master Directory Block

	headerOffset = 1024
	headerSize   = 512
)

// Special file IDs (CNIDs).
const (
	CNIDRootParent    = 1
	CNIDRootFolder    = 2
	CNIDExtents       = 3
	CNIDCatalog       = 4
	CNIDBadBlocks     = 5
	CNIDAllocation    = 6
	CNIDStartup       = 7
	CNIDAttributes    = 8
	CNIDRepairCatalog = 14
	CNIDBogusExtent   = 15
	CNIDFirstUser     = 16
)

// Volume attribute bits (VolumeHeader.Attributes).
const (
	VolumeHardwareLock       = 1 << 7
	VolumeUnmounted          = 1 << 8
	VolumeSparedBlocks       = 1 << 9
	VolumeNoCacheRequired    = 1 << 10
	VolumeBootInconsistent   = 1 << 11
	VolumeCatalogIDsReused   = 1 << 12
	VolumeJournaled          = 1 << 13
	VolumeInconsistent       = 1 << 14
	VolumeSoftwareLock       = 1 << 15
	VolumeContentProtection  = 1 << 30
	VolumeUnusedNodeFixBit31 = 1 << 31
)

// hfsEpoch is 1904-01-01 00:00:00 UTC, the zero of HFS dates, in Unix seconds.
const hfsEpoch = -2082844800

// HFSTime converts an HFS+ date, seconds since 1904-01-01 00:00:00 GMT, to a time. TN1150's one
// exception, VolumeHeader.CreateDate, is in local time; convert it with the local offset of the
// machine that wrote it, which the volume does not record.
func HFSTime(t uint32) time.Time {
	return time.Unix(int64(t)+hfsEpoch, 0).UTC()
}

// Extent is an HFSPlusExtentDescriptor: a run of allocation blocks.
type Extent struct {
	StartBlock uint32
	BlockCount uint32
}

// ForkData is an HFSPlusForkData: a fork's size and its first eight extents.
type ForkData struct {
	LogicalSize uint64
	ClumpSize   uint32
	TotalBlocks uint32
	Extents     [8]Extent
}

func decodeFork(b []byte) ForkData {
	f := ForkData{
		LogicalSize: be64(b[0:]),
		ClumpSize:   be32(b[8:]),
		TotalBlocks: be32(b[12:]),
	}
	decodeExtents(&f.Extents, b[16:])
	return f
}

func decodeExtents(e *[8]Extent, b []byte) {
	for i := range e {
		e[i] = Extent{StartBlock: be32(b[8*i:]), BlockCount: be32(b[8*i+4:])}
	}
}

// VolumeHeader is the decoded HFS+ or HFSX volume header.
type VolumeHeader struct {
	Signature          uint16 // 'H+' or 'HX'
	Version            uint16 // 4 for HFS+, 5 for HFSX
	Attributes         uint32
	LastMountedVersion uint32 // '10.0' for Mac OS X, 'HFSJ' journaled, 'fsck', ...
	JournalInfoBlock   uint32
	CreateDate         uint32 // local time
	ModifyDate         uint32
	BackupDate         uint32
	CheckedDate        uint32
	FileCount          uint32
	FolderCount        uint32
	BlockSize          uint32
	TotalBlocks        uint32
	FreeBlocks         uint32
	NextAllocation     uint32
	RsrcClumpSize      uint32
	DataClumpSize      uint32
	NextCatalogID      uint32
	WriteCount         uint32
	EncodingsBitmap    uint64
	FinderInfo         [8]uint32
	AllocationFile     ForkData
	ExtentsFile        ForkData
	CatalogFile        ForkData
	AttributesFile     ForkData
	StartupFile        ForkData

	raw [headerSize]byte
}

// Raw returns the 512 bytes of the volume header as they are on disk.
func (h *VolumeHeader) Raw() [headerSize]byte { return h.raw }

// IsHFSX reports whether the volume is HFSX, which may be case-sensitive.
func (h *VolumeHeader) IsHFSX() bool { return h.Signature == sigHFSX }

func decodeVolumeHeader(b []byte) *VolumeHeader {
	h := &VolumeHeader{
		Signature:          be16(b[0:]),
		Version:            be16(b[2:]),
		Attributes:         be32(b[4:]),
		LastMountedVersion: be32(b[8:]),
		JournalInfoBlock:   be32(b[12:]),
		CreateDate:         be32(b[16:]),
		ModifyDate:         be32(b[20:]),
		BackupDate:         be32(b[24:]),
		CheckedDate:        be32(b[28:]),
		FileCount:          be32(b[32:]),
		FolderCount:        be32(b[36:]),
		BlockSize:          be32(b[40:]),
		TotalBlocks:        be32(b[44:]),
		FreeBlocks:         be32(b[48:]),
		NextAllocation:     be32(b[52:]),
		RsrcClumpSize:      be32(b[56:]),
		DataClumpSize:      be32(b[60:]),
		NextCatalogID:      be32(b[64:]),
		WriteCount:         be32(b[68:]),
		EncodingsBitmap:    be64(b[72:]),
		AllocationFile:     decodeFork(b[112:]),
		ExtentsFile:        decodeFork(b[192:]),
		CatalogFile:        decodeFork(b[272:]),
		AttributesFile:     decodeFork(b[352:]),
		StartupFile:        decodeFork(b[432:]),
	}
	for i := range h.FinderInfo {
		h.FinderInfo[i] = be32(b[80+4*i:])
	}
	copy(h.raw[:], b)
	return h
}

// BSDInfo is HFSPlusBSDInfo, a catalog record's ownership and permissions.
type BSDInfo struct {
	OwnerID    uint32
	GroupID    uint32
	AdminFlags uint8  // the super-user half of st_flags (SF_*), shifted right by 16
	OwnerFlags uint8  // the owner half of st_flags (UF_*)
	FileMode   uint16 // st_mode: type and permission bits
	// Special is iNodeNum for a hard link, the link count for an indirect node file, and the
	// device number for a device file.
	Special uint32
}

// Flags returns the BSD st_flags word.
func (b BSDInfo) Flags() uint32 { return uint32(b.AdminFlags)<<16 | uint32(b.OwnerFlags) }

func decodeBSD(b []byte) BSDInfo {
	return BSDInfo{
		OwnerID:    be32(b[0:]),
		GroupID:    be32(b[4:]),
		AdminFlags: b[8],
		OwnerFlags: b[9],
		FileMode:   be16(b[10:]),
		Special:    be32(b[12:]),
	}
}

// BSD st_flags bits HFS+ uses.
const (
	flagUFCompressed = 0x20 // UF_COMPRESSED
)

// Catalog record types.
const (
	recFolder       = 1
	recFile         = 2
	recFolderThread = 3
	recFileThread   = 4
)

// Catalog record flags (Record.Flags).
const (
	FileLocked       = 0x0001
	ThreadExists     = 0x0002
	HasAttributes    = 0x0004
	HasSecurity      = 0x0008
	HasFolderCount   = 0x0010
	HasLinkChain     = 0x0020
	HasChildLink     = 0x0040
	HasDateAdded     = 0x0080
	FastDevPinned    = 0x0100
	DoNotFastDevPin  = 0x0200
	FastDevCandidate = 0x0400
	AutoCandidate    = 0x0800
)

// Record is a catalog folder or file record. It is what fs.FileInfo.Sys returns.
type Record struct {
	IsFolder bool
	CNID     uint32 // folderID or fileID
	ParentID uint32 // the CNID of the folder that holds the record's key
	Name     string // the POSIX name, as the macOS VFS presents it
	Flags    uint16

	Valence     uint32 // folders: number of entries
	FolderCount uint32 // folders: number of subfolders, when Flags has HasFolderCount

	CreateDate       uint32
	ContentModDate   uint32
	AttributeModDate uint32
	AccessDate       uint32
	BackupDate       uint32

	BSD BSDInfo

	// UserInfo is FileInfo or FolderInfo, FinderInfo is ExtendedFileInfo or
	// ExtendedFolderInfo: the 32 bytes of Finder information.
	UserInfo   [16]byte
	FinderInfo [16]byte

	TextEncoding uint32

	DataFork     ForkData // files only
	ResourceFork ForkData // files only

	// Link is set when the record was reached through a hard link: it is the link's own
	// record, while the receiver describes the target it resolves to.
	Link *Record
}

// Type returns a file's Finder type code, such as 'TEXT', 'slnk' or 'hlnk'.
func (r *Record) Type() uint32 {
	if r.IsFolder {
		return 0
	}
	return be32(r.UserInfo[0:])
}

// Creator returns a file's Finder creator code, such as 'ttxt', 'rhap' or 'hfs+'.
func (r *Record) Creator() uint32 {
	if r.IsFolder {
		return 0
	}
	return be32(r.UserInfo[4:])
}

// FinderFlags returns the Finder flags word of FileInfo or FolderInfo.
func (r *Record) FinderFlags() uint16 { return be16(r.UserInfo[8:]) }

// Created, Modified, Changed and Accessed return the record's dates. Changed is the attribute
// modification date, the ctime macOS reports.
func (r *Record) Created() time.Time  { return HFSTime(r.CreateDate) }
func (r *Record) Modified() time.Time { return HFSTime(r.ContentModDate) }
func (r *Record) Changed() time.Time  { return HFSTime(r.AttributeModDate) }
func (r *Record) Accessed() time.Time { return HFSTime(r.AccessDate) }
func (r *Record) Backup() time.Time   { return HFSTime(r.BackupDate) }

const (
	folderRecordSize = 88
	fileRecordSize   = 248
)

// decodeRecord decodes a folder or file record; ok is false for a thread or unknown record.
func decodeRecord(b []byte) (r *Record, ok bool, err error) {
	if len(b) < 2 {
		return nil, false, errShortRecord
	}
	switch int16(be16(b)) {
	case recFolder:
		if len(b) < folderRecordSize {
			return nil, false, errShortRecord
		}
		r = &Record{IsFolder: true, Valence: be32(b[4:]), CNID: be32(b[8:]), FolderCount: be32(b[84:])}
	case recFile:
		if len(b) < fileRecordSize {
			return nil, false, errShortRecord
		}
		r = &Record{CNID: be32(b[8:])}
		r.DataFork = decodeFork(b[88:])
		r.ResourceFork = decodeFork(b[168:])
	default:
		return nil, false, nil
	}
	r.Flags = be16(b[2:])
	r.CreateDate = be32(b[12:])
	r.ContentModDate = be32(b[16:])
	r.AttributeModDate = be32(b[20:])
	r.AccessDate = be32(b[24:])
	r.BackupDate = be32(b[28:])
	r.BSD = decodeBSD(b[32:])
	copy(r.UserInfo[:], b[48:64])
	copy(r.FinderInfo[:], b[64:80])
	r.TextEncoding = be32(b[80:])
	return r, true, nil
}

func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func be64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }

func fourCC(s string) uint32 {
	return uint32(s[0])<<24 | uint32(s[1])<<16 | uint32(s[2])<<8 | uint32(s[3])
}
