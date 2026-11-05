#!/bin/sh
# Build the complete deployment bundle with the pinned local Go toolchain.
set -eu
cd "$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
arch=${1:-arm64}
case "$arch" in arm64|amd64) ;; *) printf '%s\n' 'usage: sh build.sh [arm64|amd64]' >&2; exit 2 ;; esac
export CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOTOOLCHAIN=local GOWORK=off
mkdir -p bin
go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o bin/home-control ./cmd/home-control
(
  cd tools/dreame-bridge
  go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o ../../bin/dreame-reader ./cmd/dreame-bridge
  go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o ../../bin/dreame-provision ./cmd/dreame-provision
)
sha256sum bin/home-control bin/dreame-reader bin/dreame-provision
