#!/usr/bin/env python3
"""Restores the home locations of every block a journal holds to what they were before.

A copy of an attached image taken after fsync has its journal committed, but macOS has by then
written the same blocks home too, so replaying it changes nothing. A guest killed between the
two leaves the home locations stale. This makes that state from two real images: CLEAN, the image
before the changes, and DIRTY, the copy taken while attached. Every block the journal's pending
transactions hold is copied from CLEAN into DIRTY, except the volume header, whose home copy is
written at mount and says the volume is not cleanly unmounted. The result is written to OUT.
macOS's own replay of OUT is the golden listing, so the construction is checked by the kernel.

Usage: revert.py CLEAN DIRTY OUT
"""
import struct, sys

clean = open(sys.argv[1], "rb").read()
out = bytearray(open(sys.argv[2], "rb").read())

def hfs_partition(d):
    if d[:2] == b"ER":
        for i in range(1, 64):
            e = d[512 * i:512 * i + 512]
            if e[:2] != b"PM":
                break
            if e[48:56] == b"Apple_HF":
                return struct.unpack(">I", e[8:12])[0] * 512
    return 0

off = hfs_partition(out)
vh = out[off + 1024:off + 1536]
bs = struct.unpack(">I", vh[40:44])[0]
jib = off + struct.unpack(">I", vh[12:16])[0] * bs
jo, js = struct.unpack(">QQ", out[jib + 36:jib + 52])
J = off + jo
hdr = out[J:J + 48]
order = "<" if struct.unpack("<I", hdr[:4])[0] == 0x4A4E4C78 else ">"
_, _, start, end, size, blhdr, _, jhs = struct.unpack(order + "IIQQQIII", hdr[:44])

def read(pos, n):
    b = b""
    while n > 0:
        if pos >= size:
            pos = jhs + (pos - size)
        k = min(n, size - pos)
        b += out[J + pos:J + pos + k]
        pos += k
        n -= k
    return b

pos, restored = start, 0
while pos != end:
    h = read(pos, blhdr)
    _, num, used = struct.unpack(order + "HHi", h[:8])
    for i in range(1, num):
        bnum, bsize = struct.unpack(order + "QI", h[16 * (i + 1):16 * (i + 1) + 12])
        if bnum == 0xFFFFFFFFFFFFFFFF:
            continue
        a = off + bnum * jhs
        if a <= off + 1024 < a + bsize:
            continue  # the volume header
        out[a:a + bsize] = clean[a:a + bsize]
        restored += 1
    pos += used
    if pos >= size:
        pos = jhs + (pos - size)
open(sys.argv[3], "wb").write(out)
print("restored %d journaled blocks" % restored, file=sys.stderr)
