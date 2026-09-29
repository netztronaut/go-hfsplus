#!/usr/bin/env python3
"""Populates a mounted HFS+ image with what the reader's tests need.

Run by mkfixtures.sh on macOS, as the user, on a volume attached read-write with -owners on.
Usage: populate.py MOUNTPOINT CASE_SENSITIVE(0|1)
"""
import errno, os, subprocess, sys, tempfile, unicodedata

root, case_sensitive = sys.argv[1], sys.argv[2] == "1"
os.chdir(root)

def write(path, data, mode=None):
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    with open(path, "wb") as f:
        f.write(data)
    if mode is not None:
        os.chmod(path, mode)

def text(n, seed):
    words = [b"alpha", b"beta", b"gamma", b"delta", b"epsilon", b"zeta", b"eta", b"theta"]
    out, i = [], seed
    while sum(map(len, out)) < n:
        i = (i * 1103515245 + 12345) & 0x7FFFFFFF
        out.append(words[i % len(words)] + (b"\n" if i % 11 == 0 else b" "))
    return b"".join(out)[:n]

def noise(n, seed):
    out, x = bytearray(), seed or 1
    while len(out) < n:
        x ^= (x << 13) & 0xFFFFFFFFFFFFFFFF
        x ^= x >> 7
        x ^= (x << 17) & 0xFFFFFFFFFFFFFFFF
        out += x.to_bytes(8, "little")
    return bytes(out[:n])

# A directory hard link, as Time Machine makes them; journaled volumes only.
os.makedirs("dirlink-src/target/inner", exist_ok=True)
write("dirlink-src/target/inner/f.txt", b"in a hard-linked directory\n")
os.makedirs("dirlink-dst", exist_ok=True)
try:
    os.link("dirlink-src/target", "dirlink-dst/linked")
except OSError as e:
    print("directory hard link not created:", e, file=sys.stderr)

# An installed-looking system: the file detection reads, and a blessed folder.
write("System/Library/CoreServices/SystemVersion.plist", b"""<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>ProductBuildVersion</key>
	<string>8S165</string>
	<key>ProductName</key>
	<string>Mac OS X</string>
	<key>ProductVersion</key>
	<string>10.4.11</string>
</dict>
</plist>
""")
write("System/Library/CoreServices/BootX", noise(3000, 7))
os.makedirs("private/etc", exist_ok=True)
os.makedirs("private/var/log", exist_ok=True)
os.makedirs("private/tmp", exist_ok=True)
write("private/etc/hosts", b"127.0.0.1\tlocalhost\n")
os.symlink("private/etc", "etc")
os.symlink("private/var", "var")
os.symlink("private/tmp", "tmp")

# Nested directories, empty ones, an empty file, modes.
write("a/b/c/d/e/deep.txt", b"deep\n")
write("a/b/file.txt", text(1234, 1))
os.makedirs("empty-dir", exist_ok=True)
write("empty-file", b"")
write("modes/exec", b"#!/bin/sh\necho hi\n", 0o755)
write("modes/private", b"secret\n", 0o600)
write("modes/setuid", b"x", 0o4755)
os.makedirs("modes/sticky", exist_ok=True)
os.chmod("modes/sticky", 0o1777)
os.mkfifo("modes/fifo")

# Symbolic links: relative, absolute (within the volume), to a directory, a chain, broken, a loop.
os.makedirs("links", exist_ok=True)
os.symlink("../a/b/file.txt", "links/rel")
os.symlink("/a/b/file.txt", "links/abs")
os.symlink("../a", "links/dir")
os.symlink("rel", "links/chain")
os.symlink("nonexistent", "links/broken")
os.symlink("loop2", "links/loop1")
os.symlink("loop1", "links/loop2")
os.symlink("../../../../a/b/file.txt", "links/above-root")

# Hard links to a file, in the same and another directory, and a directory hard link.
write("hard/orig.txt", b"hard linked\n")
os.link("hard/orig.txt", "hard/link1.txt")
os.makedirs("hard/sub", exist_ok=True)
os.link("hard/orig.txt", "hard/sub/link2.txt")

# Extended attributes: inline, larger than a node holds (fork records), Finder info, a resource fork.
write("xattr/file.txt", b"has attributes\n")
subprocess.run(["xattr", "-w", "user.small", "hello", "xattr/file.txt"], check=True)
subprocess.run(["xattr", "-wx", "user.binary", "00ff00ff", "xattr/file.txt"], check=True)
import ctypes
libc = ctypes.CDLL(None, use_errno=True)
def setxattr(path, name, value):
    if libc.setxattr(path.encode(), name.encode(), value, len(value), 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "setxattr " + name)
setxattr("xattr/file.txt", "user.medium", text(2000, 3))
setxattr("xattr/file.txt", "user.large", noise(20000, 4))
setxattr("xattr/file.txt", "user.huge", text(100000, 5))
os.makedirs("xattr/dir", exist_ok=True)
setxattr("xattr/dir", "user.ondir", b"directory attribute")
finder = b"TEXTttxt" + bytes(24)
write("xattr/typed.txt", b"typed\n")
setxattr("xattr/typed.txt", "com.apple.FinderInfo", finder)
write("rsrc/file.txt", b"data fork\n")
with open("rsrc/file.txt/..namedfork/rsrc", "wb") as f:
    f.write(noise(5000, 6))
write("rsrc/only-rsrc", b"")
with open("rsrc/only-rsrc/..namedfork/rsrc", "wb") as f:
    f.write(text(300, 9))

# Names: precomposed and decomposed input, Hangul, a colon, case.
for name in ["précomposé.txt", unicodedata.normalize("NFD", "décomposé.txt"),
             "한국어.txt", "a:b.txt", "Ωhm.txt", "Ångström", "Ź̖.txt",
             "emoji-\U0001F600.txt", "space name.txt", "trailing.", "README"]:
    write("names/" + name, name.encode())
if case_sensitive:
    write("names/readme", b"lower case\n")

# Compressed files, as ditto --hfsCompression writes them: attribute-held and resource-fork-held,
# a multi-chunk one, and one that does not compress.
with tempfile.TemporaryDirectory() as src:
    write(os.path.join(src, "small.txt"), text(1500, 10))
    write(os.path.join(src, "medium.txt"), text(40000, 11))
    write(os.path.join(src, "large.txt"), text(300000, 12))
    write(os.path.join(src, "zeros.bin"), bytes(200000))
    write(os.path.join(src, "random.bin"), noise(20000, 13))
    subprocess.run(["ditto", "--hfsCompression", src, "compressed"], check=True)
# The types ditto does not write (it writes LZVN, 7 and 8), and stored-uncompressed chunks.
subprocess.run([sys.executable, os.path.join(os.path.dirname(os.path.abspath(__file__)), "compress.py"), "compressed/made"], check=True)

# A file fragmented past the eight extents of its catalog record: fill the volume with one-block
# files, delete every other one, and write into the holes.
os.makedirs("fill", exist_ok=True)
st = os.statvfs(".")
block = st.f_frsize
n = 0
try:
    while True:
        with open("fill/%05d" % n, "wb") as f:
            f.write(bytes(block))
        n += 1
except OSError as e:
    if e.errno != errno.ENOSPC:
        raise
    try:
        os.unlink("fill/%05d" % n)
    except FileNotFoundError:
        pass
# HFS+ keeps free blocks in reserve that an ordinary user cannot fill; they are contiguous, so the
# file must be larger than the reserve to be forced into the holes.
st = os.statvfs(".")
reserve = st.f_bfree - st.f_bavail
for i in range(0, n, 2):
    os.unlink("fill/%05d" % i)
frag = text(block * (reserve + 24), 14)
write("fragmented.bin", frag)
for i in range(1, n, 2):
    os.unlink("fill/%05d" % i)
os.rmdir("fill")
subprocess.run(["sync"], check=True)
