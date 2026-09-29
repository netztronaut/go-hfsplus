#!/usr/bin/env python3
"""Prints the golden listing of a mounted volume, as macOS presents it.

One line per entry, sorted by path bytes, tab-separated:
path kind perm uid gid nlink ino size mtime content xattrs
kind is d, f, l or p; perm is octal including setuid, setgid and sticky; content is the SHA-256
of a file's contents or the target of a link; xattrs are name=sha256 pairs in listxattr order.
The reader's test prints the same listing from the image and compares.
Usage: listing.py MOUNTPOINT
"""
import ctypes, hashlib, os, stat, sys

libc = ctypes.CDLL(None, use_errno=True)
XATTR_NOFOLLOW = 1

def listxattr(p):
    n = libc.listxattr(p.encode(), None, 0, XATTR_NOFOLLOW)
    if n < 0:
        raise OSError(ctypes.get_errno(), "listxattr " + p)
    buf = ctypes.create_string_buffer(n)
    n = libc.listxattr(p.encode(), buf, n, XATTR_NOFOLLOW)
    return [x.decode() for x in buf.raw[:n].split(b"\0") if x]

def getxattr(p, name):
    n = libc.getxattr(p.encode(), name.encode(), None, 0, 0, XATTR_NOFOLLOW)
    if n < 0:
        raise OSError(ctypes.get_errno(), "getxattr " + name)
    buf = ctypes.create_string_buffer(n)
    n = libc.getxattr(p.encode(), name.encode(), buf, n, 0, XATTR_NOFOLLOW)
    return buf.raw[:n]

root = sys.argv[1]
entries = []
for dirpath, dirnames, filenames in os.walk(root, followlinks=False):
    for name in dirnames + filenames:
        entries.append(os.path.join(dirpath, name))
entries.append(root)

def rel(p):
    r = os.path.relpath(p, root)
    return "." if r == "." else r

lines = []
for p in entries:
    st = os.lstat(p)
    mode = st.st_mode
    if stat.S_ISDIR(mode):
        kind, content, size = "d", "-", 0
    elif stat.S_ISLNK(mode):
        kind, content, size = "l", os.readlink(p), st.st_size
    elif stat.S_ISFIFO(mode):
        kind, content, size = "p", "-", st.st_size
    else:
        with open(p, "rb") as f:
            content = hashlib.sha256(f.read()).hexdigest()
        kind, size = "f", st.st_size
    xs = []
    for x in listxattr(p):
        v = getxattr(p, x)
        xs.append("%s=%s" % (x, hashlib.sha256(v).hexdigest()[:16]))
    nlink = st.st_nlink if kind != "d" else 0
    lines.append((rel(p).encode("utf-8", "surrogateescape"), "\t".join([
        rel(p), kind, "%o" % stat.S_IMODE(mode), str(st.st_uid), str(st.st_gid), str(nlink),
        str(st.st_ino), str(size), str(int(st.st_mtime)), content, ",".join(xs) or "-"])))
lines.sort()
sys.stdout.write("".join(l + "\n" for _, l in lines))
