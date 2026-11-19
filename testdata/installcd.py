#!/usr/bin/env python3
"""Puts an HFS+ volume on a disc laid out as the Mac OS X 10.2 install CD (6C115) is.

The CD's Driver Descriptor Record says 2048-byte blocks, and it carries two partition maps: ten
entries at 512-byte strides counting 512-byte blocks, and nine at 2048-byte strides counting
2048-byte blocks. The 512 map's entries 4 and 8, at 2048 and 4096, are Apple_Void and double as
the 2048 map's first two, so walking either map at the other's stride meets entries whose counts
disagree. Both maps describe the same partitions; the layout, status words and driver descriptors
below are the CD's, with the volume's size in place of its own. The driver and patch partitions
are left zero, since nothing reads their code.

Usage: installcd.py VOLUME DISC
VOLUME is a raw HFS+ volume (hdiutil create -layout NONE); its size must be a multiple of 2048.
"""
import struct, sys

vol = open(sys.argv[1], "rb").read()
if len(vol) % 2048:
    sys.exit("the volume is not a multiple of 2048 bytes")
hfs = len(vol) // 512  # in 512-byte blocks
free = 64

# (name, type, start, count, status, boot size, processor) in 512-byte blocks, as the 512 map has
# them; the 2048 map has the same partitions, but for Apple_Void and Apple_Free, in 2048-byte
# blocks.
parts = [
    ("Apple", "Apple_partition_map", 1, 63, 0x17, 0, ""),
    ("Macintosh", "Apple_Driver43", 64, 56, 0x37F, 9216, ""),
    ("Macintosh", "Apple_Driver43_CD", 120, 140, 0x37F, 50176, ""),
    ("", "Apple_Void", 0, 0, 0, 0, ""),
    ("Macintosh", "Apple_Driver_ATAPI", 260, 56, 0x37F, 9728, ""),
    ("Macintosh", "Apple_Driver_ATAPI", 316, 140, 0x37F, 61952, ""),
    ("Patch Partition", "Apple_Patches", 456, 512, 0x01, 0, ""),
    ("", "Apple_Void", 0, 0, 0, 0, ""),
    ("Mac_OS_X", "Apple_HFS", 968, hfs, 0x4000007F, 0, "powerpc"),
    ("", "Apple_Free", 968 + hfs, free, 0, 0, ""),
]
size = (968 + hfs + free) * 512
disc = bytearray(size)

# The DDR: 2048-byte blocks and the CD's four driver descriptors (block, size in 2048-byte
# blocks, type).
drivers = [(16, 5, 1), (30, 25, 0xFFFF), (65, 5, 0x0701), (79, 31, 0xF8FF)]
struct.pack_into(">2sHIHHIH", disc, 0, b"ER", 2048, size // 2048, 1, 1, 0, len(drivers))
for i, d in enumerate(drivers):
    struct.pack_into(">IHH", disc, 18 + 8 * i, *d)


def entry(off, count, name, typ, start, n, data, status, boot, proc):
    struct.pack_into(">2sHIII32s32sIIIIII", disc, off, b"PM", 0, count, start, n,
                     name.encode("mac_roman"), typ.encode(), 0, data, status, 0, boot, 0)
    struct.pack_into(">16s", disc, off + 120, proc.encode())


# The 2048 map first: its entries 1 and 2 are the 512 map's Apple_Void entries 4 and 8.
i = 3
for name, typ, start, n, status, boot, proc in parts:
    if typ in ("Apple_Void", "Apple_Free"):
        continue
    if typ == "Apple_partition_map":
        start, n = 1, 15
    else:
        start, n = start // 4, n // 4
    entry(i * 2048, 9, name, typ, start, n, n if typ != "Apple_partition_map" else 63, status, boot, proc)
    i += 1
assert i == 10
for i, (name, typ, start, n, status, boot, proc) in enumerate(parts, 1):
    entry(i * 512, len(parts), name, typ, start, n, n, status, boot, proc)

disc[968 * 512 : (968 + hfs) * 512] = vol
open(sys.argv[2], "wb").write(disc)
