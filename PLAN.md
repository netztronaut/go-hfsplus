# Requirements for a read-only Apple Partition Map and HFS+ reader

What an external Go library has to do for this repository to read a PowerPC Mac OS X disk, and a
pre-APFS Intel one, from the host. It is written to be handed to whoever builds the library: the
reasons are here so that a requirement can be argued with rather than guessed at.

## Why read-only is enough

docs/design.md's "7.3 The ESP, and why PowerPC needs no equivalent" settles it: on PowerPC the
orchestrator needs to *read* the machine's disk, never write it. Open Firmware boots the blessed
HFS+ volume directly, so there is no bootloader to install; Open Firmware variables are supplied at
each start by `-prom-env`; and the one thing that looks like it needs a host-written record -- "did
the install finish?" -- is answered by what Apple's installer leaves: a blessed volume and a
`SystemVersion.plist`. On x86 the only volume the host writes is the ESP, which is FAT. So nothing
in this project writes HFS+, and a library that cannot is a feature: it cannot damage a machine.

If a host-written record on PowerPC is ever needed after all, the designed fallback is an
orchestrator-owned FAT32 partition inside the APM (docs/implementation-plan.md's "M10 — PowerPC"
risk table), not an HFS+ writer.

## What this repository has today, and what the library sits beside

- `internal/disk/rawdisk` parses a GPT from the head of a raw disk, read-only, with no filesystem
  interpretation. `internal/disk/apfs` counts the volumes of an APFS container from its superblock.
- There is no FAT code in this module yet. The proof of concept used `github.com/diskfs/go-diskfs`
  (v1.9.4) for the ESP; the rewrite has not taken it across (`TODO.md`'s "The record that an install
  finished does not live on the machine's disk"). `detect.ESPApple` takes an injected `ReadFile`.
- go-diskfs has GPT, MBR, FAT12/16/32, ext4, ISO 9660 and squashfs. It has **neither the Apple
  Partition Map nor HFS+**, so the library is both, not only HFS+.

The library must therefore not assume go-diskfs: its core takes an `io.ReaderAt`. An adapter to
go-diskfs's `partition.Table` and `filesystem.FileSystem` interfaces is welcome as a separate
package, so that the core carries no dependency on it.

## Who uses it, and for what

| Use | Releases | What it needs from the library |
| --- | --- | --- |
| Is this PowerPC disk installed, erased or blank? | 10.0-10.5 PPC (M10) | APM, HFS+ volume header, blessed IDs, one file read |
| Which release is installed? | same | `/System/Library/CoreServices/SystemVersion.plist` |
| Is this Intel HFS+ disk installed or only erased? | 10.4-10.13 x86, which this project installs onto JHFS+ | the same, from a partition `rawdisk` found in a GPT |

The third row is a live problem today, not a future one: a JHFS+ disk probes as `unknown`, because
`internal/disk/apfs` reads APFS containers and an HFS+ volume is "partitioned, but not that". On
2026-09-22 that cost an install (`internal/installer/releases/legacyintel/steps.go`, `eraseDisk`'s
comment), and docs/engineering-approach.md records why `unknown` must be treated as installed. An
HFS+ reader turns `unknown` into `erased` or `installed` for every Intel release before Mojave.

## Functional requirements

Priorities: **must** is needed for the PowerPC milestone; **should** is needed for the Intel use or
for robustness; **may** is welcome and not required.

### Apple Partition Map

1. **Must** read the Driver Descriptor Record at block 0 (signature `ER`): block size and block
   count. Absence of `ER` with a `PM` at offset 512 is still a map.
2. **Must** read every partition entry (signature `PM`), using the first entry's `pmMapBlkCnt` as
   the entry count, and expose for each: name, type string, start and length (in bytes as well as
   in blocks), status flags, and the logical data start and count.
3. **Must** return each partition as an `*io.SectionReader` over the disk, so the HFS+ reader
   never sees disk offsets.
4. **Must** handle a device block size other than 512 in the DDR (2048 on optical media), with the
   entry stride and the unit of `pmPyPartStart` determined from the data, not assumed; which of the
   two a real Apple disc uses is settled against a real image, not a document.
5. **Must** recognise at least `Apple_partition_map`, `Apple_HFS`, `Apple_HFSX`, `Apple_Free`,
   `Apple_Driver*`, `Apple_Patches`, `Apple_Bootstrap`, `Apple_UFS`, and pass any other type string
   through unchanged.
6. **Must not** accept an entry that points outside the disk, overlaps the map itself, or claims
   more entries than fit; each is a typed error naming the entry.

### HFS+ and HFSX volumes

7. **Must** read the volume header at offset 1024 of the partition: signature `H+` (version 4)
   or `HX` (version 5), block size, block counts, attributes, `lastMountedVersion`, dates,
   `journalInfoBlock`, `finderInfo`, and the five special-file fork records. Expose both decoded
   fields and the raw 512 bytes.
8. **Must** follow an HFS wrapper: an HFS Master Directory Block (`BD`) whose `drEmbedSigWord` is
   `H+` holds the real volume at `drAlBlSt * 512 + drEmbedExtent.startBlock * drAlBlkSiz`. Disks
   initialised by the 10.0-10.2 era tools may carry one.
9. **Must** read the catalog B-tree: header node, index and leaf nodes of the header's node size,
   folder, file and thread records.
10. **Must** read a fork's extents beyond the eight in its catalog record from the extents overflow
    B-tree, for the data fork and the resource fork.
11. **Must** compare names as the volume does: HFS+ uses case-insensitive comparison with Apple's
    `FastUnicodeCompare` table (TN1150); HFSX uses the key compare type in the catalog header
    (`0xCF` case-folding, `0xBC` binary).
12. **Must** convert a Go string path to the volume's stored form: names are UTF-16 in Apple's
    decomposed form, and a lookup of a precomposed `é` has to find a decomposed one. Carry Apple's
    decomposition table; `golang.org/x/text`'s NFD is not the same table.
13. **Must** map the catalog's `/` in a name to `:` in the POSIX path it presents, as the macOS VFS
    does, and back.
14. **Must** expose the volume's `finderInfo` words, decoded as bless(8) writes them: the blessed
    System Folder's directory ID, the blessed boot file's ID, the Mac OS 9 and Mac OS X blessed
    folder IDs, and the 64-bit volume identifier -- each also resolved to a path when it is not 0.
    "Not blessed" is an ordinary answer, not an error. What each word holds on a PowerPC 10.4 disk
    is confirmed against a live install before the orchestrator relies on it.
15. **Must** resolve symbolic links (`slnk`/`rhap` files, the target in the data fork), relative
    and absolute, within the volume: `/etc`, `/var` and `/tmp` are links into `/private`. A bounded
    number of hops; a loop is an error.
16. **Should** resolve file hard links (`hlnk`/`hfs+` files pointing into the private
    `HFS+ Private Data` directory by link ID) and, for 10.5 and later, directory hard links.
17. **Should** read extended attributes from the attributes B-tree: inline, fork and extent records.
18. **Should** read `decmpfs`-compressed files transparently -- the `com.apple.decmpfs` attribute,
    types 1 and 3 (inline), 4 (zlib in the resource fork), 7 and 8 (LZVN), 11 and 12 (LZFSE) --
    because Intel releases from 10.6 install parts of `/System` compressed. Which files are
    compressed on each release is measured, not assumed. A compression type the library does not
    know is a typed error, never the compressed bytes presented as the file. PowerPC releases
    predate `decmpfs`, so this is not needed for M10.
19. **Should** read resource forks as their own stream.
20. **May** walk the catalog by CNID (parent chain to path), for turning a blessed ID into a path
    without a directory walk.

### The journal and a volume that was not unmounted cleanly

The orchestrator reads a disk after the guest stops, and a guest that crashed or was killed --
every pod stop does that today (`TODO.md`'s "A pod stop kills QEMU before the guest has been given
its grace period") -- leaves a journal with transactions nobody replayed. Reading the B-trees
without them can see a half-written catalog.

21. **Must** report whether the volume is journaled, whether it was cleanly unmounted
    (`kHFSVolumeUnmountedBit`), and whether its journal holds transactions (journal header `start`
    != `end`). The journal header's byte order is the writer's: big-endian from a PowerPC,
    little-endian from an Intel Mac; the `endian` field says which, and both must be read.
22. **Must**, for a volume with unreplayed transactions, either replay them into an in-memory
    overlay of blocks that every later read goes through, or refuse to open it with a typed error
    unless the caller asks for the on-disk state explicitly. It must never write the replay back.
23. **Should** implement the overlay rather than the refusal: a crashed guest is the case detection
    exists for.

### API shape

24. **Must** take an `io.ReaderAt` and a size, for the disk and for a volume. Opening files,
    block-device ioctls for the size and sector alignment are the caller's.
25. **Must** present a volume as an `io/fs.FS` that also implements `fs.StatFS`, `fs.ReadDirFS`
    and `fs.ReadFileFS`, so `fs.ReadFile(vol, "System/Library/CoreServices/SystemVersion.plist")`
    works and `testing/fstest.TestFS` can check it. `fs.FileInfo.Sys()` returns the catalog record:
    CNID, BSD owner, mode and flags, Finder info, type and creator, all four dates.
26. **Must** offer no method that writes, not even one that returns an error. The go-diskfs
    adapter, which must implement write methods, lives in its own package.
27. **Should** return typed, `errors.Is`-able errors: not this filesystem, corrupt (with the
    structure, node or offset), not found, unsupported feature, journal not replayed.
28. **Should** let the caller cancel a long operation: a directory walk or a large read checks a
    `context.Context` between nodes or extents.

A sketch, not a prescription:

```go
m, err := apm.Read(disk, size)                  // *apm.Map, or apm.ErrNoMap
for _, p := range m.Partitions { ... p.Type ... p.Section() ... }

v, err := hfsplus.Open(p.Section(), hfsplus.Options{Journal: hfsplus.Replay})
v.Header()                                      // decoded volume header, and Raw()
v.Blessed()                                     // hfsplus.Bless{SystemFolder, BootFile, ...}, ok
v.Journal()                                     // journaled, clean, pending transactions
fs.ReadFile(v, "System/Library/CoreServices/SystemVersion.plist")
```

## Non-functional requirements

29. **Pure Go, no cgo**, building for linux/amd64, linux/arm64 and darwin. Go 1.25.
30. **Dependencies:** the standard library for the partition map and the filesystem core. LZVN
    and LZFSE may bring a dependency, or live behind an interface the caller fills, so that the
    core stays dependency-free.
31. **Bounded memory.** The orchestrator shares a memory cgroup with the guest's RAM, which fills
    the container to within a few hundred MiB of its limit during an install; a probe that read
    512 MiB got the whole container OOM-killed on 2026-09-14 (`rawdisk.ReadHead`'s comment). The
    library reads only what a request needs, streams file contents through `io.Reader`, and caps
    any node cache at a size the caller sets. Peak memory is stated and tested.
32. **Never panics, never loops forever, on any input.** Every offset and length is checked against
    the partition before it is read; B-tree depth is bounded (TN1150's maximum), a node visited
    twice in one walk is corruption, extent chains and link hops are bounded.
33. **Torn reads are expected.** The same device can be read while a guest has it mounted
    read-write; the answer may be wrong, and must then be an error rather than a crash. The
    orchestrator relies on an answer only when the guest is stopped.
34. **No logging, no globals.** Errors carry the context; the orchestrator logs them in its own
    format.
35. **Licence** MIT, BSD or Apache-2.0.

## Tests and fixtures

36. Small checked-in images, compressed, made on macOS with `hdiutil create` for each of HFS+,
    JHFS+, HFSX and case-sensitive JHFS+, in an APM layout (`-layout SPUD`) and a GPT one: nested
    directories; a file fragmented past eight extents so the overflow tree is used; hard links;
    symlinks, including a loop; extended attributes; files compressed with `ditto --hfsCompression`;
    names with decomposed and precomposed characters, with `/` and `:`. Each image has a golden
    listing produced by macOS itself from `hdiutil attach -readonly` of the same image, and the
    library's listing must equal it.
37. A dirty-journal image, made by writing to a mounted image and detaching it forcibly, with the
    golden listing of the same image after macOS replayed it.
38. An HFS wrapper image, from real 10.0-10.2 era media if one can be found, otherwise synthesised
    and labelled as such.
39. Fuzz targets for the DDR and partition entries, the volume header and MDB, a B-tree node, the
    journal header and block list, and the `decmpfs` header; each run for ten minutes in the
    library's CI without a panic or a hang.
40. Real disks, as optional tests that skip themselves when the image is absent (images of whole
    installs do not belong in a repository): an installed 10.4 PowerPC disk, and an Intel disk this
    project installed onto JHFS+ (Snow Leopard, High Sierra).

## Done when

- From a raw image of an installed 10.4 PowerPC disk: the APM lists the `Apple_HFS` partition, the
  volume is blessed, the blessed folder resolves to a path, and `SystemVersion.plist` reads and
  names 10.4.x.
- From the same disk erased by `diskutil eraseDisk JHFS+` and nothing else: a volume, not blessed,
  and no `SystemVersion.plist` -- so erased and installed are told apart.
- From a JHFS+ partition `rawdisk` found in an Intel GPT this project installed: the same two
  answers, with compressed files read correctly.
- From a volume killed mid-write: the journal is reported, and the reads agree with macOS's own
  after its replay.
- Every fuzz target has run for ten minutes with no finding, and peak memory reading the metadata
  of a 64 GiB volume is under the stated cap.

## Out of scope

Writing anything, including journal replay back to disk; HFS standard volumes other than as a
wrapper; UFS; APFS (`internal/disk/apfs` covers what detection needs); DMG, UDIF and sparse
bundles; ISO 9660 and the hybrid layouts of install discs. Parsing the property list is the
caller's.

## What this repository does with it

Not part of the library, listed so the boundary is clear: an `apm` and an `hfsplus` source in
`internal/detect`, declared PowerPC and Intel-before-APFS respectively (docs/implementation-plan.md's
"M5 — Detection"); `internal/disk/apfs`'s `unknown` verdict split into `erased` and `installed` for
HFS+; and `hoststate.Reader` for PowerPC, as docs/design.md's "7.3 The ESP, and why PowerPC needs no
equivalent" sketches it.
