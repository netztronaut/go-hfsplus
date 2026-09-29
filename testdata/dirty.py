#!/usr/bin/env python3
"""Changes a mounted journaled volume and flushes it to the image with F_FULLFSYNC, so that a copy
of the image taken now has the changes in its journal, as transactions start..end the journal
header has not yet been moved past. (macOS writes the blocks home too before the copy can see the
journal; revert.py undoes that.)
Usage: dirty.py MOUNTPOINT
"""
import fcntl, os, sys

F_FULLFSYNC = 51
os.chdir(sys.argv[1])
for i in range(40):
    os.makedirs("after/d%02d" % i)
    with open("after/d%02d/f" % i, "w") as f:
        f.write("written after the last clean unmount %d\n" % i)
        f.flush()
        os.fsync(f.fileno())
os.rename("a/b/file.txt", "a/b/renamed.txt")
os.unlink("empty-file")
os.rmdir("empty-dir")
os.symlink("after/d00/f", "after-link")
fd = os.open(".", os.O_RDONLY)
fcntl.fcntl(fd, F_FULLFSYNC)
os.close(fd)
