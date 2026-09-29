#!/bin/sh
# Makes the checked-in HFS+ images and their golden listings. macOS only; run from anywhere.
#
# Each image is made with hdiutil, populated by populate.py through the macOS kernel, detached,
# attached again read-only, and listed by listing.py: the golden listing is what macOS itself
# presents for the image. The reader's tests must produce the same listing from the image.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
out=$here/images
work=$(mktemp -d)
mkdir -p "$out" "$work/m"
cleanup() { hdiutil detach -quiet -force "$work/m" 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT

attach() { hdiutil attach -quiet -nobrowse -owners on -mountpoint "$work/m" "$@"; }
detach() { hdiutil detach -quiet "$work/m"; }

# wanted NAME: whether NAME was asked for; no arguments asks for all. (hdiutil recognises raw
# images by their .dmg extension.)
wanted() {
	[ $# -eq 0 ] && return 0
	case " $only " in *" $1 "*) return 0 ;; esac
	return 1
}
only="$*"

# image NAME FS LAYOUT CASE_SENSITIVE POPULATE
image() {
	name=$1 fs=$2 layout=$3 cs=$4 populate=$5
	[ -z "$only" ] || wanted "$name" $only || return 0
	rm -f "$work/$name.dmg"
	hdiutil create -quiet -size 16m -fs "$fs" -volname "$name" -layout "$layout" -o "$work/$name.dmg"
	if [ "$populate" = 1 ]; then
		attach "$work/$name.dmg"
		python3 "$here/populate.py" "$work/m" "$cs"
		bless --folder "$work/m/System/Library/CoreServices" --file "$work/m/System/Library/CoreServices/BootX"
		detach
	fi
	attach -readonly "$work/$name.dmg"
	python3 "$here/listing.py" "$work/m" >"$out/$name.golden"
	bless --info "$work/m" 2>&1 | sed "s#/.*/m/#/#; s#/.*/m\$#/#" >"$out/$name.bless" || true
	detach
	gzip -9 -n -c "$work/$name.dmg" >"$out/$name.img.gz"
}

for layout in SPUD GPTSPUD; do
	case $layout in SPUD) l=apm ;; GPTSPUD) l=gpt ;; esac
	image "hfsplus-$l" "HFS+" "$layout" 0 1
	image "jhfsplus-$l" "Journaled HFS+" "$layout" 0 1
	image "hfsx-$l" "Case-sensitive HFS+" "$layout" 1 1
	image "jhfsx-$l" "Case-sensitive Journaled HFS+" "$layout" 1 1
done
# Erased, as diskutil eraseDisk JHFS+ leaves a disk: a volume and nothing else.
image "erased-jhfsplus-apm" "Journaled HFS+" SPUD 0 0
image "erased-jhfsplus-gpt" "Journaled HFS+" GPTSPUD 0 0

# A journaled volume with transactions nobody replayed, as a killed guest leaves it. The image is
# populated and cleanly detached, attached again and changed, and copied while still attached;
# revert.py then restores the pre-change contents of the blocks the journal holds, because by the
# time the copy can see the journal macOS has written those blocks home too. macOS will not attach
# a dirty volume read-only, so the golden listing is of a copy it attached read-write, which
# replays the journal, then detached and attached read-only.
name=dirty-jhfsplus-apm
if [ -z "$only" ] || wanted "$name" $only; then
image "$name" "Journaled HFS+" SPUD 0 1
cp "$work/$name.dmg" "$work/$name-clean.dmg"
attach "$work/$name.dmg"
python3 "$here/dirty.py" "$work/m"
cp "$work/$name.dmg" "$work/$name-snapshot.dmg"
detach
python3 "$here/revert.py" "$work/$name-clean.dmg" "$work/$name-snapshot.dmg" "$work/$name-dirty.dmg"
cp "$work/$name-dirty.dmg" "$work/$name-replayed.dmg"
attach "$work/$name-replayed.dmg"
detach
attach -readonly "$work/$name-replayed.dmg"
python3 "$here/listing.py" "$work/m" >"$out/$name.golden"
detach
gzip -9 -n -c "$work/$name-dirty.dmg" >"$out/$name.img.gz"
fi
