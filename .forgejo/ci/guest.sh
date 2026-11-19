#!/bin/sh
# In the macOS guest: guest.sh <job dir on the share>
#
# Unpacks <job dir>/source.tgz into a temporary directory and runs vet and the tests of both modules
# there. Go is the newest release of the go.mod's version, from go.dev, kept with the module and build
# caches in ~/Library/Caches/go-hfsplus-ci so later runs fetch nothing.
set -eu
job=$1
cache=$HOME/Library/Caches/go-hfsplus-ci
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
tar -C "$work" -xzf "$job/source.tgz"

minor=$(sed -n 's/^go \([0-9]*\.[0-9]*\).*/\1/p' "$work/go.mod")
case $(uname -m) in
x86_64) arch=amd64 ;;
arm64) arch=arm64 ;;
*) echo "unknown architecture $(uname -m)" >&2; exit 1 ;;
esac
# go.dev lists releases newest first.
version=$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all' |
	grep -o "\"go$minor\.[0-9]*\"" | head -n 1 | tr -d '"')
[ -n "$version" ] || { echo "no Go release for $minor" >&2; exit 1; }
goroot=$cache/$version/go
if [ ! -x "$goroot/bin/go" ]; then
	mkdir -p "$cache/$version.tmp"
	curl -fsSL "https://go.dev/dl/$version.darwin-$arch.tar.gz" | tar -C "$cache/$version.tmp" -xzf -
	rm -rf "$cache/$version"
	mv "$cache/$version.tmp" "$cache/$version"
fi
export PATH="$goroot/bin:$PATH" GOPATH="$cache/gopath" GOCACHE="$cache/gocache" GOFLAGS=-modcacherw GOTOOLCHAIN=local

cd "$work"
go version
sw_vers
test -z "$(gofmt -l .)"
go vet ./...
go test ./...
cd diskfs
go vet ./...
go test ./...
