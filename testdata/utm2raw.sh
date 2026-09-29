#!/bin/sh
# Converts the disk of a UTM virtual machine, a .utm bundle or a ZIP of one, to a raw image for
# the real-disk tests, with qemu-img (Homebrew's qemu). The image is sparse: it takes the space of
# what the guest wrote, not the virtual size.
#
#	sh testdata/utm2raw.sh "Mac OS X 10.4.utm.zip" /path/to/10.4.raw
#	HFSPLUS_PPC_IMAGES=/path/to/10.4.raw:/path/to/10.5.raw go test -run TestPowerPCReleases -v ./hfsplus
set -eu
src=${1:?usage: utm2raw.sh VM.utm[.zip] IMAGE}
img=${2:?usage: utm2raw.sh VM.utm[.zip] IMAGE}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
case $src in
*.zip)
	unzip -q -j "$src" '*/Data/*.qcow2' -x '__MACOSX/*' -d "$work"
	set -- "$work"/*.qcow2
	;;
*)
	set -- "$src"/Data/*.qcow2
	;;
esac
if [ $# -ne 1 ] || [ ! -f "$1" ]; then
	echo "utm2raw.sh: expected one qcow2 disk in $src" >&2
	exit 1
fi
qemu-img convert -O raw "$1" "$img"
