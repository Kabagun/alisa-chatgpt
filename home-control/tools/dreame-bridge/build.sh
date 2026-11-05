#!/bin/sh
# Reproducible stdlib-only Linux binaries. Run from any directory with Go1.27.3.
set -eu
cd "$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
arch=${1:-arm64}
case "$arch" in arm64|amd64) ;; *) exit 2 ;; esac
export CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOTOOLCHAIN=local GOWORK=off
mkdir -p "dist/linux-$arch"
go build -trimpath -ldflags='-s -w -buildid=' -o "dist/linux-$arch/dreame-bridge" ./cmd/dreame-bridge
go build -trimpath -ldflags='-s -w -buildid=' -o "dist/linux-$arch/dreame-provision" ./cmd/dreame-provision
sha256sum "dist/linux-$arch/dreame-bridge" "dist/linux-$arch/dreame-provision"
