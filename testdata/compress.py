#!/usr/bin/env python3
"""Writes decmpfs-compressed files of the types ditto does not write, as afsctool does: the data
in a com.apple.decmpfs attribute or the resource fork, an empty data fork, UF_COMPRESSED. The
golden listing reads them back through the kernel, which checks the layout.

Usage: compress.py DIR
Types: 3 (zlib, attribute), 4 (zlib, resource fork), 11 (LZFSE, attribute), 12 (LZFSE,
resource fork), 1 (uncompressed, attribute).
"""
import ctypes, os, struct, sys, zlib

libc = ctypes.CDLL(None, use_errno=True)
libcompression = ctypes.CDLL("/usr/lib/libcompression.dylib")
COMPRESSION_LZFSE = 0x801
UF_COMPRESSED = 0x20
CHUNK = 65536

def lzfse(b):
    out = ctypes.create_string_buffer(len(b) + 4096)
    n = libcompression.compression_encode_buffer(out, len(out), b, len(b), None, COMPRESSION_LZFSE)
    if n == 0:
        raise RuntimeError("lzfse encode failed")
    return out.raw[:n]

def setxattr(path, name, value):
    if libc.setxattr(path.encode(), name.encode(), value, len(value), 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "setxattr %s %s" % (name, path))

def header(t, size):
    return struct.pack("<4sIQ", b"fpmc", t, size)

def rsrc_fork_cmpf(data, encode):
    # A resource fork holding one 'cmpf' resource: the chunk table and zlib chunks.
    chunks = [encode(data[i:i + CHUNK]) for i in range(0, len(data), CHUNK)]
    table = struct.pack("<I", len(chunks))
    off = 4 + 8 * len(chunks)
    body = b""
    for c in chunks:
        table += struct.pack("<II", off, len(c))
        off += len(c)
        body += c
    res = table + body
    data_part = struct.pack(">I", len(res)) + res
    fork_map = bytes(16) + struct.pack(">IHHHH", 0, 0, 0, 28, 50) + struct.pack(">H4sHH", 0, b"cmpf", 0, 10) \
        + struct.pack(">hHBBBBI", 1, 0xFFFF, 0, 0, 0, 0, 0)
    head = struct.pack(">IIII", 0x100, 0x100 + len(data_part), len(data_part), len(fork_map))
    return head + bytes(0x100 - 16) + data_part + fork_map

def rsrc_fork_table(data, encode):
    # Types 8 and 12: nchunks+1 little-endian offsets, then the chunks.
    chunks = [encode(data[i:i + CHUNK]) for i in range(0, len(data), CHUNK)]
    off = 4 * (len(chunks) + 1)
    table = b""
    for c in chunks:
        table += struct.pack("<I", off)
        off += len(c)
    table += struct.pack("<I", off)
    return table + b"".join(chunks)

def zlib9(b):
    return zlib.compress(b, 9)

def stored(marker):
    # A chunk kept uncompressed behind the marker byte its algorithm's streams cannot start with.
    return lambda b: bytes([marker]) + b

def make(path, t, data, encode=None):
    with open(path, "wb"):
        pass
    if t in (1, 3, 11):
        payload = {1: data, 3: zlib.compress(data, 9), 11: lzfse(data)}[t]
        setxattr(path, "com.apple.decmpfs", header(t, len(data)) + payload)
    else:
        encode = encode or {4: zlib9, 8: None, 12: lzfse}[t]
        fork = rsrc_fork_cmpf(data, encode) if t == 4 else rsrc_fork_table(data, encode)
        with open(path + "/..namedfork/rsrc", "wb") as f:
            f.write(fork)
        setxattr(path, "com.apple.decmpfs", header(t, len(data)))
    if libc.chflags(path.encode(), UF_COMPRESSED) != 0:
        raise OSError(ctypes.get_errno(), "chflags " + path)

def text(n, seed):
    words = [b"lorem", b"ipsum", b"dolor", b"sit", b"amet", b"consectetur", b"adipiscing"]
    out, i = [], seed
    while sum(map(len, out)) < n:
        i = (i * 1103515245 + 12345) & 0x7FFFFFFF
        out.append(words[i % len(words)] + (b"\n" if i % 13 == 0 else b" "))
    return b"".join(out)[:n]

d = sys.argv[1]
os.makedirs(d, exist_ok=True)
make(os.path.join(d, "type1-raw-attr.txt"), 1, text(500, 1))
make(os.path.join(d, "type3-zlib-attr.txt"), 3, text(3000, 2))
make(os.path.join(d, "type4-zlib-rsrc.txt"), 4, text(200000, 3))
make(os.path.join(d, "type11-lzfse-attr.txt"), 11, text(3000, 4))
make(os.path.join(d, "type12-lzfse-rsrc.txt"), 12, text(200000, 5))
x, raw = 88172645463325252, bytearray()
while len(raw) < 100000:
    x ^= (x << 13) & 0xFFFFFFFFFFFFFFFF
    x ^= x >> 7
    x ^= (x << 17) & 0xFFFFFFFFFFFFFFFF
    raw += x.to_bytes(8, "little")
raw = bytes(raw[:100000])
make(os.path.join(d, "type4-stored-chunks.bin"), 4, raw, stored(0xFF))
make(os.path.join(d, "type8-stored-chunks.bin"), 8, raw, stored(0x06))
make(os.path.join(d, "type12-stored-chunks.bin"), 12, raw, stored(0xFF))
