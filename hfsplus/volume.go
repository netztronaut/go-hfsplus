// Package hfsplus reads HFS+ and HFSX volumes, the file systems of Mac OS X before APFS, from an
// io.ReaderAt. It never writes: it has no method that does, and journal replay is applied to an
// in-memory overlay that every read goes through.
//
// A Volume is an io/fs.FS, so fs.ReadFile, fs.WalkDir and testing/fstest work on it; it also
// exposes what an fs.FS cannot: the volume header, the blessed folders, the journal, extended
// attributes and resource forks. Paths are the POSIX paths the macOS VFS presents: names in
// Apple's decomposed Unicode, a "/" in a stored name shown as ":", symbolic and hard links
// resolved within the volume.
//
// The layout is Apple's Technical Note TN1150, "HFS Plus Volume Format", and, for the journal,
// xnu's vfs_journal.c.
package hfsplus

import (
	"context"
	"fmt"
	"io"
	"strconv"
)

// JournalMode says what Open does with a journal that holds transactions nobody replayed, which is
// what a guest that crashed or was killed leaves.
type JournalMode int

const (
	// Replay applies the pending transactions to an in-memory overlay that every read goes
	// through, so the volume reads as macOS would read it after mounting it. Nothing is written.
	Replay JournalMode = iota
	// Refuse makes Open fail with ErrJournalNotReplayed when transactions are pending.
	Refuse
	// OnDisk reads the blocks as they are on disk, ignoring the journal. Journal.Pending still
	// reports whether that state is stale.
	OnDisk
)

// DefaultCacheSize is the B-tree node cache size when Options.CacheSize is 0.
const DefaultCacheSize = 4 << 20

// MaxSymlinkHops bounds the symbolic links one path resolution follows, as MAXSYMLINKS does.
const MaxSymlinkHops = 32

// Options configure Open.
type Options struct {
	// Journal says what to do with unreplayed transactions. The zero value replays them.
	Journal JournalMode
	// CacheSize caps the memory the B-tree node cache holds, in bytes. 0 means
	// DefaultCacheSize; a negative value disables the cache.
	CacheSize int
	// MaxJournalBlocks caps the number of journal blocks a replay may overlay; the overlay index
	// takes about 50 bytes per block, and the block contents stay on disk. 0 means no cap beyond
	// the journal's size.
	MaxJournalBlocks int
	// Decompressors adds or replaces decmpfs decompressors, keyed by compression type. The
	// built-in ones handle types 3 and 4 (zlib), 7 and 8 (LZVN), 11 and 12 (LZFSE).
	Decompressors map[uint32]Decompressor
	// ShowPrivate lists the metadata macOS hides from directory listings: the hard link
	// directories "\x00\x00\x00\x00HFS+ Private Data" (shown with U+2400 for each NUL) and
	// ".HFS+ Private Directory Data\r", and the journal files ".journal" and
	// ".journal_info_block".
	ShowPrivate bool
}

// Volume is an open HFS+ or HFSX volume. It is safe for concurrent use.
type Volume struct {
	*volume
	ctx context.Context
}

type volume struct {
	dev    *device
	offset int64 // partition offset of the HFS+ volume: 0, or the embedded volume in a wrapper
	size   int64 // bytes of the HFS+ volume

	hdr         *VolumeHeader
	wrapper     *Wrapper
	blockSize   uint32
	totalBlocks uint32
	journal     *JournalInfo

	cache      *nodeCache
	extents    *btree
	catalog    *btree
	attributes *btree // nil when the volume has no attributes file

	caseSensitive bool
	opts          Options
	decompressors map[uint32]Decompressor

	root *Record // the root folder's record, named "."

	// CNIDs of what macOS hides from the root directory, 0 when absent.
	privateDir    uint32 // "\x00\x00\x00\x00HFS+ Private Data", file hard link targets
	privateDirDir uint32 // ".HFS+ Private Directory Data\r", directory hard link targets
	journalFile   uint32
	journalInfo   uint32
}

// Wrapper describes the HFS Master Directory Block an HFS+ volume was found embedded in.
type Wrapper struct {
	// Offset and Length locate the embedded HFS+ volume within the partition, in bytes.
	Offset int64
	Length int64
	// The MDB fields that locate it: drAlBlSt, drAlBlkSiz and drEmbedExtent.
	AllocationStart uint16
	BlockSize       uint32
	EmbedStart      uint16
	EmbedCount      uint16
	Raw             [headerSize]byte
}

// Open opens the HFS+ or HFSX volume in r, which is size bytes long: a partition, such as an
// apm.Partition's Section or a GPT partition, not a whole disk.
func Open(r io.ReaderAt, size int64, opts Options) (*Volume, error) {
	return OpenContext(context.Background(), r, size, opts)
}

// OpenContext is Open with a context that bounds the work Open does. The Volume returned uses
// context.Background; see WithContext.
func OpenContext(ctx context.Context, r io.ReaderAt, size int64, opts Options) (*Volume, error) {
	if size < headerOffset+headerSize {
		return nil, ErrNotHFSPlus
	}
	v := &volume{dev: &device{r: r, size: size}, size: size, opts: opts}

	var buf [headerSize]byte
	if err := v.dev.readFull(buf[:], headerOffset, "volume header"); err != nil {
		return nil, err
	}
	if be16(buf[:]) == sigHFS {
		w, err := decodeWrapper(buf[:], size)
		if err != nil {
			return nil, err
		}
		v.wrapper = w
		v.offset, v.size = w.Offset, w.Length
		if err := v.dev.readFull(buf[:], v.offset+headerOffset, "volume header"); err != nil {
			return nil, err
		}
	}
	hdr, err := v.checkHeader(buf[:])
	if err != nil {
		return nil, err
	}

	replay := opts.Journal == Replay
	info, jerr := openJournal(v.dev, hdr, v.offset, replay, opts.MaxJournalBlocks)
	switch {
	case jerr != nil && opts.Journal == OnDisk:
		info = &JournalInfo{Journaled: true, CleanlyUnmounted: hdr.Attributes&VolumeUnmounted != 0, Err: jerr}
	case jerr != nil:
		return nil, jerr
	case opts.Journal == Refuse && info.Pending:
		return nil, fmt.Errorf("%w: %d transactions, %d blocks", ErrJournalNotReplayed, info.Transactions, info.Blocks)
	}
	v.journal = info
	if info.Replayed {
		// The volume header is itself journaled.
		if err := v.dev.readFull(buf[:], v.offset+headerOffset, "volume header"); err != nil {
			return nil, err
		}
		if hdr, err = v.checkHeader(buf[:]); err != nil {
			return nil, err
		}
	}
	v.hdr = hdr
	v.blockSize = hdr.BlockSize
	v.totalBlocks = hdr.TotalBlocks

	switch {
	case opts.CacheSize == 0:
		v.cache = newNodeCache(DefaultCacheSize)
	default:
		v.cache = newNodeCache(opts.CacheSize)
	}
	v.decompressors = map[uint32]Decompressor{
		3: decompressZlib, 4: decompressZlib,
		7: decompressLZVN, 8: decompressLZVN,
		11: decompressLZFSE, 12: decompressLZFSE,
	}
	for t, d := range opts.Decompressors {
		v.decompressors[t] = d
	}

	if err := v.openTrees(ctx); err != nil {
		return nil, err
	}
	vol := &Volume{volume: v, ctx: context.Background()}
	if err := v.findPrivate(ctx); err != nil {
		return nil, err
	}
	return vol, nil
}

func decodeWrapper(b []byte, size int64) (*Wrapper, error) {
	w := &Wrapper{
		BlockSize:       be32(b[20:]),
		AllocationStart: be16(b[28:]),
		EmbedStart:      be16(b[126:]),
		EmbedCount:      be16(b[128:]),
	}
	copy(w.Raw[:], b)
	if sig := be16(b[124:]); sig != sigHFSPlus {
		return nil, fmt.Errorf("%w: an HFS standard volume, embedded signature %#04x", ErrNotHFSPlus, sig)
	}
	if w.BlockSize == 0 || w.BlockSize%512 != 0 {
		return nil, corrupt("HFS wrapper", headerOffset, "allocation block size %d", w.BlockSize)
	}
	w.Offset = int64(w.AllocationStart)*512 + int64(w.EmbedStart)*int64(w.BlockSize)
	w.Length = int64(w.EmbedCount) * int64(w.BlockSize)
	if w.Length < headerOffset+headerSize || w.Offset+w.Length > size {
		return nil, corrupt("HFS wrapper", headerOffset, "embedded volume at %d, %d bytes, in a %d-byte partition", w.Offset, w.Length, size)
	}
	return w, nil
}

func (v *volume) checkHeader(b []byte) (*VolumeHeader, error) {
	h := decodeVolumeHeader(b)
	switch {
	case h.Signature == sigHFSPlus && h.Version == 4, h.Signature == sigHFSX && h.Version == 5:
	case h.Signature == sigHFSPlus || h.Signature == sigHFSX:
		return nil, unsupported("volume header version %d with signature %#04x", h.Version, h.Signature)
	default:
		return nil, ErrNotHFSPlus
	}
	off := v.offset + headerOffset
	switch {
	case h.BlockSize < 512 || h.BlockSize&(h.BlockSize-1) != 0:
		return nil, corrupt("volume header", off, "block size %d", h.BlockSize)
	case uint64(h.TotalBlocks)*uint64(h.BlockSize) > uint64(v.size):
		return nil, corrupt("volume header", off, "%d blocks of %d bytes in a %d-byte volume", h.TotalBlocks, h.BlockSize, v.size)
	case h.FreeBlocks > h.TotalBlocks:
		return nil, corrupt("volume header", off, "%d free blocks of %d", h.FreeBlocks, h.TotalBlocks)
	case h.CatalogFile.TotalBlocks == 0 || h.ExtentsFile.TotalBlocks == 0:
		return nil, corrupt("volume header", off, "no catalog or extents file")
	case h.Attributes&VolumeJournaled != 0 && h.JournalInfoBlock >= h.TotalBlocks:
		return nil, corrupt("volume header", off, "journal info block %d of %d", h.JournalInfoBlock, h.TotalBlocks)
	}
	return h, nil
}

// Tree identities in the node cache.
const (
	treeExtents = iota
	treeCatalog
	treeAttributes
)

func (v *volume) openTrees(ctx context.Context) error {
	ef, err := v.newFork("extents overflow file", v.hdr.ExtentsFile, nil)
	if err != nil {
		return err
	}
	if v.extents, err = v.openBTree(ctx, treeExtents, "extents overflow B-tree", ef); err != nil {
		return err
	}
	cf, err := v.newFork("catalog file", v.hdr.CatalogFile, v.overflow(CNIDCatalog, forkData))
	if err != nil {
		return err
	}
	if v.catalog, err = v.openBTree(ctx, treeCatalog, "catalog B-tree", cf); err != nil {
		return err
	}
	v.caseSensitive = v.hdr.IsHFSX() && v.catalog.compareType == keyCompareBinary
	if v.hdr.IsHFSX() && v.catalog.compareType != keyCompareBinary && v.catalog.compareType != keyCompareFolding {
		return corruptNode(v.catalog.name, 0, "key compare type %#02x", v.catalog.compareType)
	}
	if v.hdr.AttributesFile.TotalBlocks > 0 && v.hdr.AttributesFile.LogicalSize > 0 {
		af, err := v.newFork("attributes file", v.hdr.AttributesFile, v.overflow(CNIDAttributes, forkData))
		if err != nil {
			return err
		}
		if v.attributes, err = v.openBTree(ctx, treeAttributes, "attributes B-tree", af); err != nil {
			return err
		}
	}
	return nil
}

var (
	privateDirName    = append([]uint16{0, 0, 0, 0}, utf16Of("HFS+ Private Data")...)
	privateDirDirName = utf16Of(".HFS+ Private Directory Data\r")
	journalFileName   = utf16Of(".journal")
	journalInfoName   = utf16Of(".journal_info_block")
)

func utf16Of(s string) []uint16 {
	u := make([]uint16, len(s))
	for i := range len(s) {
		u[i] = uint16(s[i])
	}
	return u
}

func (v *volume) findPrivate(ctx context.Context) error {
	root, err := v.byID(ctx, CNIDRootFolder)
	if err != nil {
		return err
	}
	if !root.IsFolder {
		return corrupt("catalog B-tree", -1, "the root CNID is a file")
	}
	root.Name = "."
	v.root = root
	find := func(name []uint16, folder bool) (uint32, error) {
		r, err := v.lookup(ctx, CNIDRootFolder, name)
		if err != nil || r == nil || r.IsFolder != folder {
			return 0, err
		}
		return r.CNID, nil
	}
	if v.privateDir, err = find(privateDirName, true); err != nil {
		return err
	}
	if v.privateDirDir, err = find(privateDirDirName, true); err != nil {
		return err
	}
	if v.journal.Journaled {
		if v.journalFile, err = find(journalFileName, false); err != nil {
			return err
		}
		if v.journalInfo, err = find(journalInfoName, false); err != nil {
			return err
		}
	}
	return nil
}

// WithContext returns a view of the volume whose operations check ctx between B-tree nodes and
// between extents, and stop with ctx's error once it is done.
func (v *Volume) WithContext(ctx context.Context) *Volume {
	return &Volume{volume: v.volume, ctx: ctx}
}

// Header returns the decoded volume header, after journal replay when there was one.
func (v *Volume) Header() *VolumeHeader {
	h := *v.hdr
	return &h
}

// Wrapper returns the HFS wrapper the volume is embedded in, or nil.
func (v *Volume) Wrapper() *Wrapper { return v.wrapper }

// Journal reports whether the volume is journaled, whether it was cleanly unmounted, and whether
// its journal holds transactions.
func (v *Volume) Journal() JournalInfo { return *v.journal }

// CaseSensitive reports whether names are compared as binary UTF-16, which is an HFSX volume
// created case-sensitive.
func (v *Volume) CaseSensitive() bool { return v.caseSensitive }

// Name returns the volume name, the name of the root folder's thread record.
func (v *Volume) Name() (string, error) {
	_, name, _, err := v.thread(v.ctx, CNIDRootFolder)
	if err != nil {
		return "", err
	}
	return nameFromHFS(name), nil
}

// Bless is the volume header's Finder information, decoded as bless(8) writes it.
type Bless struct {
	// SystemFolder is finderInfo[0], the blessed System Folder's directory ID: on Mac OS X,
	// /System/Library/CoreServices.
	SystemFolder uint32
	// BootFile is finderInfo[1], the blessed boot file: BootX on PowerPC, boot.efi on Intel.
	BootFile uint32
	// OpenFolder is finderInfo[2], the folder the Finder opens when the volume mounts.
	OpenFolder uint32
	// OS9Folder is finderInfo[3], a Mac OS 9 System Folder, and OSXFolder finderInfo[5], the
	// Mac OS X one, which bless records when both are present.
	OS9Folder uint32
	OSXFolder uint32
	// VolumeID is finderInfo[6] and [7], the 64-bit volume identifier.
	VolumeID uint64

	// The IDs above resolved to paths, relative to the volume root; empty when the ID is 0 or
	// no catalog record has it.
	SystemFolderPath string
	BootFilePath     string
	OpenFolderPath   string
	OS9FolderPath    string
	OSXFolderPath    string
}

// Blessed decodes the volume's blessed folder and file. ok reports whether a System Folder is
// blessed; a volume that is not is an ordinary answer, not an error.
func (v *Volume) Blessed() (b Bless, ok bool, err error) {
	fi := v.hdr.FinderInfo
	b = Bless{
		SystemFolder: fi[0], BootFile: fi[1], OpenFolder: fi[2], OS9Folder: fi[3], OSXFolder: fi[5],
		VolumeID: uint64(fi[6])<<32 | uint64(fi[7]),
	}
	for _, x := range []struct {
		id   uint32
		path *string
	}{
		{b.SystemFolder, &b.SystemFolderPath},
		{b.BootFile, &b.BootFilePath},
		{b.OpenFolder, &b.OpenFolderPath},
		{b.OS9Folder, &b.OS9FolderPath},
		{b.OSXFolder, &b.OSXFolderPath},
	} {
		if x.id == 0 {
			continue
		}
		p, err := v.PathOf(x.id)
		switch {
		case err == nil:
			*x.path = p
		case isNotExist(err):
		default:
			return b, false, err
		}
	}
	return b, b.SystemFolder != 0 || b.OSXFolder != 0, nil
}

func cnidName(prefix string, n uint32) []uint16 {
	return utf16Of(prefix + strconv.FormatUint(uint64(n), 10))
}
