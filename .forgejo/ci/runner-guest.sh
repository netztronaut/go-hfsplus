#!/bin/sh
# On the Forgejo runner beside the macOS guest: runner-guest.sh
#
# Runs .forgejo/ci/guest.sh in the guest over ssh (127.0.0.1:22, the pod's key from the ssh-agent),
# with the checkout in ./src put on the 9p share (/share in the runner, /Volumes/share in the
# guest). The same shape as the other macos components' runner-guest.sh.
set -eu
job=go-hfsplus-test-$$-$(date +%s)
share=/share/$job
mkdir -p "$share"
chmod 777 "$share"
ssh_guest() {
	ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o LogLevel=ERROR \
		-o ConnectTimeout=15 -o ServerAliveInterval=30 macos@127.0.0.1 "$@"
}
# What the guest writes on the share arrives as root, which the runner's rm cannot take: the guest
# removes it.
cleanup() {
	ssh_guest "rm -rf /Volumes/share/$job" 2>/dev/null || true
	rm -rf "$share" 2>/dev/null || true
}
trap cleanup EXIT

cp src/.forgejo/ci/guest.sh "$share/"
tar -C src --exclude .git -czf "$share/source.tgz" .
chmod -R a+r "$share"

n=0
until ssh_guest true; do
	n=$((n + 1))
	[ "$n" -lt 40 ] || { echo "the guest did not answer ssh" >&2; exit 1; }
	sleep 5
done
ssh_guest "sh /Volumes/share/$job/guest.sh /Volumes/share/$job 2>&1"
