#!/bin/sh
# Compile only. Configure an existing compatible CC and Go toolchain first.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
: "${GO:=go}"
: "${CC:=cc}"
command -v "$GO" >/dev/null
command -v "$CC" >/dev/null
mkdir -p "$ROOT/build"
cd "$ROOT"
export CC CGO_ENABLED=1
"$GO" build -mod=readonly -trimpath -buildvcs=false -o build/probe .
