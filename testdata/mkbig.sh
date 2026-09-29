#!/bin/sh
# Makes a 64 GiB raw disk image, sparse, with a GPT and a journaled HFS+ volume holding 100,000
# files in nested directories, for the peak-memory test (TestMemoryBigVolume). macOS only. The
# image is not checked in; point HFSPLUS_BIG_IMAGE at it:
#
#	sh testdata/mkbig.sh /path/to/big.img
#	HFSPLUS_BIG_IMAGE=/path/to/big.img go test -run TestMemoryBigVolume -v ./hfsplus
#
# The file is sparse: it takes the space of what is written, about 1 GiB, not 64 GiB.
set -eu
img=${1:?usage: mkbig.sh IMAGE}
work=$(mktemp -d)
dev=
cleanup() {
	hdiutil detach -quiet -force "$work/m" 2>/dev/null || true
	[ -n "$dev" ] && hdiutil detach -quiet -force "$dev" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT
rm -f "$img"
truncate -s 64g "$img"
dev=$(hdiutil attach -nomount -imagekey diskimage-class=CRawDiskImage "$img" | awk 'NR==1 {print $1}')
diskutil partitionDisk "$dev" GPT JHFS+ Big 100% >/dev/null
diskutil unmount "${dev}s2" >/dev/null 2>&1 || true
mkdir -p "$work/m"
diskutil mount -mountPoint "$work/m" "${dev}s2" >/dev/null
python3 - "$work/m" <<'PY'
import os, sys
root = sys.argv[1]
for i in range(100):
    for j in range(10):
        d = os.path.join(root, "d%03d" % i, "e%02d" % j)
        os.makedirs(d)
        for k in range(100):
            with open(os.path.join(d, "file-with-a-longer-name-%03d.txt" % k), "w") as f:
                f.write("%d %d %d\n" % (i, j, k))
PY
diskutil unmount "$work/m" >/dev/null
hdiutil detach -quiet "$dev"
dev=
