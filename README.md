# go-hfsplus

```sh
go get netztronaut.de/go-hfsplus
```

Read-only Apple Partition Map and HFS+/HFSX readers in pure Go, for inspecting the disk of a
PowerPC Mac OS X install, or of a pre-APFS Intel one, from the host.

- `apm` reads an Apple Partition Map: the Driver Descriptor Record, every partition entry, each
  partition as an `*io.SectionReader`.
- `hfsplus` opens an HFS+ or HFSX volume, including one embedded in an HFS wrapper, as an
  `io/fs.FS` (`fs.StatFS`, `fs.ReadDirFS`, `fs.ReadFileFS`, `fs.ReadLinkFS`), and exposes the volume
  header, the blessed folders, the journal, extended attributes and resource forks.
- `lzvn` and `lzfse` decode the compression `decmpfs` uses; `hfsplus` reads compressed files
  transparently.
- `diskfs/` is a separate module, `netztronaut.de/go-hfsplus/diskfs`, adapting both to
  go-diskfs's `partition.Table` and `filesystem.FileSystem`, so the readers themselves depend on
  nothing but the standard library. It requires a tagged release of the readers; to work on both
  at once, `go work init . ./diskfs` (go.work is not checked in).

Nothing here writes. There is no write method, not even one that fails; the go-diskfs adapter's
write methods, which its interfaces require, return `filesystem.ErrReadonlyFilesystem`. A journal
with unreplayed transactions is replayed into an in-memory overlay that every read goes through,
never back to the disk.

```go
m, err := apm.Read(disk, size)            // apm.ErrNoMap for a disk without one
p := m.Find(apm.TypeHFS)
v, err := hfsplus.Open(p.Section(), p.Length, hfsplus.Options{}) // replays a dirty journal
b, ok, err := v.Blessed()                 // ok false: not blessed, an ordinary answer
j := v.Journal()                          // journaled, cleanly unmounted, pending, replayed
plist, err := fs.ReadFile(v, "System/Library/CoreServices/SystemVersion.plist")
```

## Paths and names

Paths are the POSIX paths the macOS VFS presents. Names are stored in UTF-16 in Apple's
decomposed form; a lookup converts the Go string the way the kernel does, so a precomposed `é`
finds a stored decomposed one, and a case-insensitive volume finds `README` as `readme`. The
decomposition and case-folding tables are Apple's (Unicode 3.2 with Apple's exclusions, and
TN1150's `FastUnicodeCompare` folding), generated from the Unicode data and verified code point by
code point against the macOS kernel (`internal/gen/unicode`). A `/` in a stored name is presented
as `:`, and back. Directory listings return names as stored, decomposed, as macOS does.

Symbolic links resolve within the volume: an absolute target starts at the volume root, so
`/etc/hosts` reads `private/etc/hosts`; `..` at the root stays there; at most 32 links are followed,
and a loop is `ErrLinkLoop`. File and directory hard links resolve to their targets in the private
directories, which, with the journal files, are hidden from listings as macOS hides them
(`Options.ShowPrivate` shows them). `fs.FileInfo.Sys()` returns the `*hfsplus.Record`: CNID, BSD
owner, mode and flags, Finder information, type and creator, the dates.

Dates are presented as `stat(2)` presents them: one before 1970, including the 0 some files on a
10.4 install carry, is 1970-01-01, as the kernel's `to_bsd_time` clamps it. `Options.RawDates`
presents them as stored instead, a 0 as 1904-01-01; the `Record` fields always hold the stored
values. `Record.BSD.Flags()` is the
stored word; `stat(2)` adds `UF_HIDDEN` for the Finder's invisible bit.

`ListXattr` and `GetXattr` present extended attributes as macOS's `listxattr(2)` does:
`com.apple.FinderInfo` and `com.apple.ResourceFork` are synthesised, `com.apple.decmpfs` of a
compressed file and the protected `com.apple.system.` attributes are hidden. `Attributes` lists
what the attributes B-tree holds.

## Robustness

The input is untrusted: a disk a guest may have left half-written, or may be writing while it is
read. Every offset and length is checked against the partition before it is read; B-tree depth is
bounded by TN1150's maximum of 16, a leaf chain that visits a node twice is corruption, extent
chains, link hops and journal scans are bounded. Errors are typed and `errors.Is`-able:
`ErrNotHFSPlus`, `ErrCorrupt` (a `*CorruptError` naming the structure, node and offset),
`fs.ErrNotExist`, `ErrUnsupported`, `ErrJournalNotReplayed`, `ErrLinkLoop`. `Volume.WithContext`
makes a walk or a large read stop between nodes and extents when its context is done. There is no
logging and no mutable global state.

## Memory

The reader holds what a request needs: the B-tree nodes on the path it is searching, a node cache
capped by `Options.CacheSize` (default 4 MiB), a bit per catalog node while walking a leaf chain,
one 64 KiB chunk of a compressed file, and the journal overlay's index, about 50 bytes per
journaled block, whose contents stay on disk. File contents stream through `io.Reader`.
`fs.ReadDir` of a directory holds its entries, about 300 bytes each.

Measured (`TestMemoryBigVolume`, on the sparse 64 GiB image `testdata/mkbig.sh` makes, read from
the file): stat-ing all 101,144 entries of the volume and reading every directory holds a peak of
4.2 MiB of heap above the baseline with the default 4 MiB cache. The test's stated bound is the
cache size plus 8 MiB; `TestMemoryFixture` checks the same bound with a 64 KiB cache on every run.

## Tests

`testdata/mkfixtures.sh` makes the checked-in images on macOS with `hdiutil`: HFS+, journaled
HFS+, case-sensitive HFSX and journaled HFSX, each in an APM (`-layout SPUD`) and a GPT, with
nested directories, a file fragmented past the eight extents of its catalog record, file and
directory hard links, symbolic links including a loop, inline and fork extended attributes, Finder
information and resource forks, files compressed by `ditto --hfsCompression`, and names with
precomposed and decomposed characters, Hangul, `:` and case variants; erased volumes; and a
journaled volume copied while mounted, whose journal holds transactions nobody replayed. Each has
a golden listing macOS produced from the attached image (`testdata/listing.py`), which the
reader's listing must equal; the dirty image's is of a copy macOS replayed. The HFS wrapper and the
case-folding HFSX catalog are synthesised in the tests from those images, and labelled so.

Fuzz targets cover the partition map, the volume header and MDB, B-tree nodes, the journal header
and block list, the `decmpfs` header and chunk tables, name conversion, and both decompressors; CI
runs each for ten minutes.

Journal replay was measured against the real thing before it was written: the header checksum
covers 44 bytes, a block list header's 32, each block's checksum its whole block, and block
numbers count `jhdr_size` units of the partition. The dirty image holds transactions whose blocks'
home locations are stale (macOS writes them home before a copy can see the journal, so
`testdata/revert.py` restores their earlier contents from the image before the changes); its
golden listing is macOS's own replay of that image, and the test checks the replay changes blocks,
matches the golden listing, and that the on-disk state lacks what only the journal holds.

The decmpfs layouts macOS does not write with `ditto` (types 1, 3, 4, 11, 12, and chunks stored
uncompressed behind their marker bytes) are written through the kernel by `testdata/compress.py`
and read back by macOS for the golden listing, so every type is checked against the kernel.

Optional tests read real disks when they are there: set `HFSPLUS_PPC_IMAGE` to a raw image of an
installed 10.4 PowerPC disk, `HFSPLUS_PPC_IMAGES` to a list of raw images of installed PowerPC
disks of any release (`testdata/utm2raw.sh` converts a UTM virtual machine's disk with
`qemu-img`), which are read entirely, `HFSPLUS_INTEL_IMAGE` to a raw Intel GPT disk with a JHFS+ install,
and `HFSPLUS_BIG_IMAGE` to the 64 GiB image `testdata/mkbig.sh` makes. `HFSPLUS_MEDIA` takes
`IMAGE=MOUNTPOINT` pairs and compares the reader with macOS on media it has attached: names, modes, sizes, modification
times, link targets and contents up to 1 MiB. On the
Mac OS X 10.5.8 install DVD (APM, 512-byte blocks, a trailing `Apple_Free` cut short by the image)
25,250 of 25,251 entries agreed, the exception a file rewritten in the image after macOS had
attached it; on the Mojave installer (APM with 2048-byte blocks) and its BaseSystem (GPT, read
from `/dev/rdisk`) every entry agreed, 50,073 of them, 37,362 compared by contents, with the
blessed folder and `boot.efi` resolved (those three runs compared names, types, sizes, link
targets and contents, before modes and times were added).

Installed PowerPC systems, 10.0.3, 10.1.5, 10.2.8, 10.3.8, 10.4.11 and 10.5.8, each on a 64 GiB
APM disk from a UTM virtual machine, were read entirely and compared with macOS attaching the same
image: every entry, 1,162,424 of them, agreed in name, mode, size, BSD flags, all four dates, link
target, contents of every file (22 GiB), resource fork and extended attribute, except the few
entries macOS would not let an unprivileged reader open (`.Spotlight-V100`, `.Trashes`, `sudo`).
10.0 to 10.2 are HFS+ in an HFS wrapper, 10.3 onwards journaled; 10.5 holds 94,711 hard links. On
all six the volume header's Finder information holds the blessed `System/Library/CoreServices`
in words 0 and 5 and 0 in words 2 and 3; word 1 is 0 but on 10.5, which blesses
`System/Library/CoreServices/boot.efi` there even on a PowerPC. The only difference the comparison found was the dates
before 1970 described above.

Not yet checked against a real disk: an Intel system disk this project installed onto JHFS+.

## Licence

MIT. The Unicode tables are derived from the Unicode Character Database; see
`hfsplus/unicode_tables.go` for its notice.
